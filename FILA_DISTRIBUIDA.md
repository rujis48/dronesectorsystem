# Fila Distribuída - Documentação

## Visão Geral

A **Fila Distribuída** é um mecanismo de replanejamento automático implementado via Raft que armazena requisições de alocação de drones quando não há drones disponíveis no cluster. O sistema processa a fila automaticamente quando drones são liberados.

---

## Arquitetura

### Componentes Principais

1. **PendingRequest**: Estrutura que representa uma requisição enfileirada
   ```go
   type PendingRequest struct {
       ID              string    // ID único da requisição
       SectorID        string    // Setor que solicitou
       Severity        string    // "easy" (1), "medium" (2), "hard" (3)
       DurationSeconds int       // Tempo estimado para conserto
       DronesNeeded    int       // Quantidade de drones necessários
       QueuedAt        time.Time // Timestamp do enfileiramento
   }
   ```

2. **FSM.queue**: Slice thread-safe que armazena requisições enfileiradas
   - Protegido por `sync.Mutex`
   - Distribuído via Raft (todos os nós têm cópia sincronizada)

3. **Comandos Raft**:
   - `queue_request`: Enfileira uma requisição explicitamente
   - `process_queue`: Processa fila e aloca drones para requisições
   - `get_queue`: Lê fila atual (somente leitura)

---

## Fluxo de Funcionamento

### Cenário 1: Solicitação com Drones Disponíveis

```
Solicitação chega (assign_with_fallback)
        ↓
Verifica drones disponíveis
        ↓
✓ Aloca drones → retorna
```

### Cenário 2: Solicitação sem Drones Disponíveis

```
Solicitação chega (assign_with_fallback)
        ↓
Verifica drones disponíveis
        ↓
✗ Sem drones disponíveis
        ↓
Cria PendingRequest
        ↓
Adiciona à fila Raft
        ↓
Retorna {queued: true, request_id: "..."}
        ↓
[Fila Distribuída com 1 requisição]
```

### Cenário 3: Processamento da Fila

```
[Líder a cada 5 segundos]
        ↓
Verifica tamanho da fila
        ↓
Se fila > 0:
  - Aplica comando "process_queue"
  - Tenta alocar drones para cada requisição
  - Remove requisições satisfeitas
  - Log de progresso
```

### Cenário 4: Liberação de Drone

```
Drone completa conserto (finish_fix)
        ↓
Muda status para "available"
        ↓
Triggered: process_queue
        ↓
Aloca drone para próxima requisição da fila
```

---

## API HTTP

### GET /queue
Retorna lista completa de requisições enfileiradas:

```json
{
  "sector_id": "sector1",
  "queue_size": 3,
  "is_leader": true,
  "queue": [
    {
      "id": "sector2-20260519120500",
      "sector_id": "sector2",
      "severity": "hard",
      "duration_seconds": 30,
      "drones_needed": 2,
      "queued_at": "2026-05-19T12:05:00Z"
    }
  ]
}
```

### GET /queue/status
Retorna estatísticas agregadas da fila:

```json
{
  "sector_id": "sector1",
  "queue_size": 2,
  "drones_available": 0,
  "drones_total": 5,
  "oldest_request_age_sec": 45,
  "is_leader": true
}
```

---

## Testes Manuais

### Teste 1: Observar Enfileiramento

1. Iniciar cluster com 3 setores:
   ```bash
   sudo docker compose up --build
   ```

2. Verificar fila vazia:
   ```bash
   curl http://localhost:7001/queue/status
   # Output: queue_size: 0
   ```

3. Em outro terminal, monitore o líder:
   ```bash
   sudo docker logs -f sector1 | grep -E "\[FILA\]|\[LÍDER\]"
   ```

4. Enviar múltiplas requisições "hard" simultaneamente (5+ requisições em paralelo)
   - Isso vai saturar o pool de 5 drones
   - Requisições extras devem ser enfileiradas

5. Verificar fila com requisições:
   ```bash
   curl http://localhost:7001/queue
   # Deve mostrar queue_size > 0
   ```

### Teste 2: Processamento de Fila

1. Com fila populada, aguarde 10 segundos
   - Drones completam consertos
   - Status muda para "available"
   - Líder processa fila automaticamente

2. Verificar logs:
   ```bash
   sudo docker logs -f sector1 | grep "Processamento:"
   # Output: [FILA] Processamento: X satisfeitas, Y restando na fila
   ```

3. Verificar `/queue/status` - `queue_size` deve diminuir

### Teste 3: Recuperação de Fila em Failover

1. Com fila populada, derrube o líder:
   ```bash
   sudo docker stop sector1
   ```

2. Novo líder é eleito (sector2 ou sector3)

3. Verificar fila no novo líder:
   ```bash
   curl http://localhost:7002/queue
   # Fila deve estar sincronizada (Raft garantiu)
   ```

4. Novo líder continua processando fila automaticamente

---

## Logs Importantes

### Enfileiramento
```
[FILA] Requisição enfileirada - ID: sector2-20260519120500 para sector2 (fila tamanho: 1)
```

### Processamento
```
[FILA] Processamento: 2 satisfeitas, 1 restando na fila
```

### Alocação Parcial
```
[FILA] Alocação parcial para sector2 - ID: xxx (1 drones, 1 pendente)
```

---

## Limitações e Considerações

1. **Ordem FIFO Estrita**: Requisições são processadas na ordem de chegada
2. **Sem Timeout Explícito**: Requisições na fila podem aguardar indefinidamente (em versão futura, adicionar TTL)
3. **Replanejamento Limitado**: Processamento a cada 5 segundos pode causar latência em cenários de alta demanda
4. **Sem Priorização Dinâmica**: Todas requisições têm mesma prioridade (em versão futura, considerar priorização por severidade)

---

## Exemplo de Fluxo Completo

```
[T=0s] Solicitação 1 (hard, 3 drones) → Aloca 3 drones (pool=5, restam 2)
[T=1s] Solicitação 2 (hard, 3 drones) → Aloca 2 drones (pool=2, restam 0)
                                        + Enfileira 1 drone faltante → FILA=[req2]
[T=2s] Solicitação 3 (medium, 2 drones) → Nenhum disponível → FILA=[req2, req3]
[T=3s] Solicitação 4 (easy, 1 drone) → Nenhum disponível → FILA=[req2, req3, req4]

[T=30s] Drone termina conserto → status=available
[T=30s] Líder processa fila → Aloca para req2 (faltava 1 drone)
        → Aloca 2 para req3
[T=30s] FILA=[req4] (req2 satisfeita, req3 satisfeita)

[T=60s] Drones terminam → Aloca para req4
        → FILA=[] (vazia!)
```

---

## Monitoramento Recomendado

Para produção, monitore:
- `queue_size` em `/queue/status`
- `oldest_request_age_sec` - alertar se > 60s
- Logs com `[FILA]` para diagnosticar problemas
- Taxa de processamento: `processed / interval`

