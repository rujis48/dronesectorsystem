# Drone Sector System

Sistema distribuído de gestão de drones utilizando consenso **Raft** para coordenação entre múltiplos setores. Cada setor é um nó Raft independente que expõe uma API HTTP para monitoramento e controlo. O líder do cluster gere a alocação de drones, processa uma fila distribuída e gere problemas automaticamente.

---

## Índice

- [Arquitetura Geral](#arquitetura-geral)
- [Estrutura do Projeto](#estrutura-do-projeto)
- [Scripts](#scripts)
  - [configure.sh](#configuresh)
  - [test_fila.sh](#test_filash)
- [Código Go](#código-go)
  - [Sectors/Sector.go](#sectorssectorgo)
  - [Drones/drone.go](#dronesdronego)
  - [Monitor/monitor.go](#monitormonitorigo)
- [Primeiros Passos](#primeiros-passos)
  - [Pré-requisitos](#pré-requisitos)
  - [Configuração do Ambiente](#configuração-do-ambiente)
  - [Construção e Execução](#construção-e-execução)
- [Monitor em Tempo Real](#monitor-em-tempo-real)
- [API HTTP](#api-http)
- [Boas Práticas](#boas-práticas)
- [Simulação de Falhas](#simulação-de-falhas)
- [Notas e Curiosidades](#notas-e-curiosidades)

---

## Arquitetura Geral

```
                    ┌─────────────────────────┐
                    │     Monitor (Terminal)    │
                    │   Descobre setores via   │
                    │   scan de portas HTTP    │
                    └────────────┬─────────────┘
                                 │ HTTP (polling)
                    ┌────────────┼─────────────┐
                    │            │              │
              ┌─────▼─────┐ ┌───▼───────┐ ┌───▼───────┐
              │  Sector 1  │ │  Sector 2  │ │  Sector 3  │
              │  (Líder)   │ │ (Follower) │ │ (Follower) │
              │  HTTP :7001│ │ HTTP :7002 │ │ HTTP :7003 │
              │  Raft :5001│ │ Raft :5002 │ │ Raft :5003 │
              └─────┬──────┘ └─────┬──────┘ └─────┬──────┘
                    │              │              │
                    └──────────────┼──────────────┘
                           Raft Consensus
                           (TCP Transport)
```

- **Raft** garante que todos os nós partilham o mesmo estado (drones, filas, comandos)
- Apenas o **líder** executa rotinas de gestão (gerar problemas, libertar drones, processar fila)
- O **monitor** é um cliente independente que descobre setores dinamicamente

---

## Estrutura do Projeto

```
dronesectorsystem/
├── Sectors/
│   ├── Sector.go          # Nó do setor (main entrypoint, Raft, HTTP API)
│   └── Dockerfile         # Imagem Docker para os setores
├── Drones/
│   └── drone.go           # FSM do Raft, lógica de drones e fila distribuída
├── Monitor/
│   └── monitor.go         # Dashboard de terminal com descoberta dinâmica
├── docker-compose.yaml    # Definição dos 3 setores padrão
├── configure.sh           # Script interativo para gerar docker-compose customizado
├── test_fila.sh           # Teste de integração da fila distribuída
├── go.mod / go.sum        # Dependências Go (hashicorp/raft v1.7.3)
└── README.md              # Este ficheiro
```

---

## Scripts

### configure.sh

Script interativo que gera um `docker-compose.yaml` customizado para ambientes multi-máquina ou com número variável de setores.

**O que faz:**

1. Pergunta quantos setores o cluster deve ter
2. Oferece duas opções de configuração de IP:
   - **Manual** — insere o IP de cada máquina individualmente (para deploy distribuída)
   - **Automático** — usa nomes Docker (`sector1`, `sector2`, ...) para ambiente local
3. Constrói a string `PEERS` no formato `ip1:5001,ip2:5002,...`
4. Pergunta quais setores correm nesta máquina específica
5. Gera o `docker-compose.yaml` apenas com os setores selecionados
6. Executa `docker compose down -v` para limpar volumes antigos

**Uso:**

```bash
chmod +x configure.sh
./configure.sh
```

**Exemplo de sessão:**

```
Quantos setores (nós) farão parte do cluster? (Ex: 3): 5
1) Digitar manualmente o IP de cada setor
2) Usar IPs automáticos (127.0.0.1 para todos)
Escolha uma opção (1 ou 2): 2

Quais setores você deseja que rodem NESTA máquina?
Setores para esta máquina: 1 2 3
```

> **Nota:** O script gera o ficheiro como `sudo docker-compose.yaml` — após gerar, renomeie para `docker-compose.yaml` se necessário.

---

### test_fila.sh

Script de teste de integração que demonstra o funcionamento da fila distribuída.

**O que faz:**

1. **Fase 1** — Verifica o estado inicial da fila
2. **Fase 2** — Envia 10 requisições `hard` em paralelo para saturar o pool de 5 drones
3. **Fase 3** — Verifica a fila após saturação (~6 segundos)
4. **Fase 4** — Monitoriza o processamento da fila a cada 5 segundos durante 60 segundos
5. **Fase 5** — Relatório final com estado da fila e drones

**Uso:**

```bash
# Certifique-se de que o cluster está a correr
chmod +x test_fila.sh
./test_fila.sh
```

**Requisitos:** `curl` e `jq` instalados na máquina local.

---

## Código Go

### Sectors/Sector.go

O coração do sistema — cada setor é uma instância deste programa.

**Responsabilidades:**

| Componente | Descrição |
|---|---|
| **Configuração Raft** | Lê `SECTOR_ID`, `BIND_ADDR`, `PEERS`, `DATA_DIR` do ambiente. Resolve hostnames Docker para IPs reais. |
| **Bootstrap** | O `sector1` inicializa o cluster Raft com a lista completa de servidores. |
| **API HTTP** | Expõe endpoints em `HTTP_PORT` para consulta de estado. |
| **Rotinas do Líder** | 4 goroutines que só executam no líder (ver abaixo). |

**Goroutines do Líder:**

1. **Monitor de Timers** (1s) — Liberta drones cujo `FinishAt` expirou. Simula 30% de chance de falha pós-conserto. Envia heartbeats e processa a fila após libertação.
2. **Processador de Fila** (5s) — Drena a fila distribuída quando há drones disponíveis.
3. **Detector de Drones Mortos** (7s) — Identifica drones com saúde crítica ou heartbeat stale (>15s) e cria substitutos.
4. **Gerador de Problemas** (5-15s aleatório) — O líder gera problemas aleatórios em setores sorteados, atribuindo drones com severidade `easy` (1 drone), `medium` (2), `hard` (3).

**Variáveis de ambiente:**

| Variável | Obrigatória | Exemplo | Descrição |
|---|---|---|---|
| `SECTOR_ID` | Sim | `sector1` | Identificador único do setor |
| `BIND_ADDR` | Sim | `sector1:5001` | Endereço para o transporte Raft |
| `PEERS` | Sim | `sector1:5001,sector2:5002` | Lista de todos os peers do cluster |
| `HTTP_PORT` | Não | `7001` | Porta do servidor HTTP (default: 7000) |
| `DATA_DIR` | Não | `/tmp/raft-sector1` | Directório para dados persistentes Raft |
| `ADVERTISE_ADDR` | Não | — | Endereço anunciado a outros nós |
| `BOOTSTRAP` | Não | `true` | Se deve fazer bootstrap do cluster |

---

### Drones/drone.go

Máquina de estados finitos (FSM) replicada via Raft. Todo o estado do sistema vive aqui.

**Estruturas principais:**

```go
Drone {
    ID, AssignedTo, Status      // "available", "fixing", "dead"
    FinishAt, Health             // "healthy", "critical"
    LastHeartbeat, ConsecutiveFailures, ConsecutiveSuccesses
}

PendingRequest {
    ID, SectorID, Severity, DurationSeconds, DronesNeeded, QueuedAt
}
```

**Comandos da FSM (operações replicadas):**

| Operação | Descrição |
|---|---|
| `assign` | Aloca N drones (por severidade) a um setor |
| `assign_with_fallback` | Tenta alocar; se insuficiente, enfileira o resto |
| `queue_request` | Enfileira explicitamente um pedido |
| `process_queue` | Processa pedidos em fila (FIFO) |
| `finish_fix` | Marca drone como disponível novamente |
| `get_drones` | Retorna cópia de todos os drones |
| `get_queue` | Retorna cópia da fila |
| `simulate_drone_failure` | 30% chance de falha; 3 falhas consecutivas = drone morto |
| `heartbeat_drone` | Reset de contadores de falha, atualiza timestamp |
| `detect_dead_drones` | Varredura de drones com saúde crítica ou stale |
| `create_drone` | Cria drone substituto para um setor |

**Funções auxiliares:**

- `NewFSM(count)` — Cria FSM com N drones iniciais em `sector1`
- `GetAvailableCount()` — Conta drones disponíveis e saudáveis
- `GetDrones()` / `GetQueue()` — Cópias thread-safe do estado
- `Snapshot()` / `Restore()` — Serialização para persistência Raft

---

### Monitor/monitor.go

Dashboard de terminal que descobre e monitoriza setores dinamicamente.

**Funcionamento:**

1. A cada ciclo (1 segundo), escaneia um intervalo de portas HTTP em paralelo
2. Setores que respondem com JSON válido (`/status`) são registados como online
3. Setores que desaparecem ficam marcados como OFFLINE durante 5 ciclos antes de serem removidos
4. Drones são deduplicados por ID (todos os setores partilham o mesmo estado Raft)
5. Setores são ordenados por ID para exibição consistente

**Variáveis de ambiente:**

| Variável | Default | Descrição |
|---|---|---|
| `MONITOR_HOST` | `localhost` | Host a escanear |
| `MONITOR_PORT_START` | `7001` | Porta inicial do intervalo |
| `MONITOR_PORT_END` | `7010` | Porta final do intervalo |

**Exemplo de execução com configuração customizada:**

```bash
MONITOR_HOST=192.168.1.100 MONITOR_PORT_START=7001 MONITOR_PORT_END=7005 go run Monitor/monitor.go
```

---

## Primeiros Passos

### Pré-requisitos

- **Docker** e **Docker Compose** instalados
- **Go 1.21+** (apenas para correr o monitor fora do container)
- **jq** e **curl** (para usar `test_fila.sh`)

### Configuração do Ambiente

**Opção A — Ambiente local padrão (3 setores):**

```bash
# O docker-compose.yaml já vem configurado para 3 setores
docker compose up --build
```

**Opção B — Ambiente customizado:**

```bash
# Use o script interativo para gerar um docker-compose.yaml personalizado
./configure.sh

# Depois inicie
docker compose up --build
```

**Opção C — Multi-máquina:**

1. Em cada máquina, execute `configure.sh` e selecione apenas os setores que devem correr nela
2. Na opção de IP, escolha "Manual" e insira os IPs reais de cada máquina
3. Certifique-se de que as portas Raft (500X) e HTTP (700X) estão acessíveis na rede

### Construção e Execução

```bash
# Construir e iniciar todos os setores
docker compose up --build

# Em background
docker compose up --build -d

# Ver logs em tempo real
docker compose logs -f

# Ver logs de um setor específico
docker compose logs -f sector1

# Parar o cluster
docker compose down

# Parar e limpar volumes (estado Raft)
docker compose down -v
```

---

## Monitor em Tempo Real

O monitor corre fora do Docker, diretamente na máquina host.

```bash
# Execução básica (escaneia portas 7001-7010 em localhost)
go run Monitor/monitor.go

# Com configuração customizada
MONITOR_HOST=docker-host-ip MONITOR_PORT_START=7001 MONITOR_PORT_END=7005 go run Monitor/monitor.go

# Ou compile primeiro
go build -o monitor ./Monitor/
./monitor
```

**O monitor mostra:**
- Estado de cada setor (ONLINE / OFFLINE / LÍDER)
- Drones em cada setor (asteriscos verdes = disponíveis, vermelhos = a consertar)
- Contagem de drones ociosos vs. a consertar
- Número de setores descobertos e intervalo de portas

---

## API HTTP

Cada setor expõe os seguintes endpoints:

| Endpoint | Método | Descrição |
|---|---|---|
| `/status` | GET | Estado do setor, lista de drones, se é líder |
| `/confirm` | GET | Health check com confirmação de recebimento |
| `/queue` | GET | Lista de requisições enfileiradas |
| `/queue/status` | GET | Estatísticas da fila (tamanho, drones disponíveis, idade do pedido mais antigo) |
| `/drones/health` | GET | Informação detalhada de saúde de cada drone |

**Exemplos:**

```bash
# Ver estado do sector1
curl http://localhost:7001/status | jq

# Verificar saúde do cluster
curl http://localhost:7001/confirm | jq

# Ver fila distribuída
curl http://localhost:7001/queue | jq

# Estatísticas rápidas
curl http://localhost:7001/queue/status | jq

# Saúde dos drones
curl http://localhost:7001/drones/health | jq
```

---

## Boas Práticas

**Antes de executar:**

1. Limpe volumes antigos se reconfigurou o cluster:
   ```bash
   docker compose down -v
   ```
2. Verifique que não há containers antigos a correr:
   ```bash
   docker ps -a | grep sector
   ```
3. Se usar `configure.sh`, confirme que o `docker-compose.yaml` gerado tem os ports e peers corretos

**Durante a execução:**

- Use o monitor para acompanhar o estado em tempo real
- Verifique os logs do líder para entender o comportamento do sistema:
  ```bash
  docker compose logs -f sector1 | grep "LÍDER"
  ```
- O líder é eleito automaticamente; se parar, outro nó assume em poucos segundos

**Ao adicionar setores novos:**

1. Adicione o novo serviço ao `docker-compose.yaml` com uma porta HTTP única
2. Atualize a variável `PEERS` em **todos** os setores existentes para incluir o novo nó
3. O monitor descobre automaticamente setores novos no intervalo de portas configurado

---

## Simulação de Falhas

```bash
# Parar um setor (simula crash)
docker stop sector2

# Reiniciar um setor
docker start sector2

# Parar o líder (força reeleição)
docker stop sector1

# Verificar quem é o novo líder
curl http://localhost:7002/confirm | jq '.is_leader'
```

**Comportamento esperado:**
- O cluster continua a funcionar com 2 de 3 nós (quorum)
- Um novo líder é eleito automaticamente
- Drones do setor parado permanecem no estado até o líder os gereir
- O monitor mostra o setor como OFFLINE e remove-o após 5 ciclos sem resposta

**Simular perda de maioria (Falha Crítica):**

```bash
docker stop sector1 sector2
```

O setor restante perde o cargo de líder e congela a aplicação de novos comandos, protegendo o sistema contra split-brain.

---

## Notas e Curiosidades

- O sistema usa **BoltDB** como backend de persistência para o Raft log e stable store. Os dados sobrevivem a restarts dos containers graças aos volumes Docker nomeados.

- A eleição de líder tipicamente demora **2-5 segundos** após a queda do líder anterior.

- O timeout de transporte Raft está configurado para **10 segundos**. Se um nó ficar inacessível por mais que isso, é considerado morto pelo cluster.

- O gerador de problemas do líder usa distribuição ponderada: **50% easy**, **30% medium**, **20% hard**.

- Drones com 3 falhas consecutivas de health check são marcados como `critical`/`dead` e são automaticamente substituídos pelo líder.

- O monitor usa **300ms de timeout** por porta na descoberta, tornando o scan de 10 portas praticamente instantâneo com goroutines.

- A fila distribuída é processada em FIFO (First In, First Out). Pedidos parcialmente satisfeitos permanecem na fila com a contagem de drones restantes atualizada.

- O `configure.sh` gera portas Raft sequenciais (5001, 5002, ...) e portas HTTP sequenciais (7001, 7002, ...). A porta HTTP é calculada como `7000 + número_do_setor`.

- Todos os endpoints HTTP incluem o header `Access-Control-Allow-Origin: *`, permitindo acesso de ferramentas de frontend ou browsers para debug.

---

## Comandos Rápidos

| Ação | Comando |
|---|---|
| Subir cluster | `docker compose up --build` |
| Subir em background | `docker compose up --build -d` |
| Parar cluster | `docker compose down` |
| Parar e limpar estado | `docker compose down -v` |
| Ver logs | `docker compose logs -f` |
| Ver logs de um setor | `docker compose logs -f sector1` |
| Monitor em tempo real | `go run Monitor/monitor.go` |
| Consultar estado | `curl http://localhost:7001/status \| jq` |
| Gerar compose customizado | `./configure.sh` |
| Testar fila distribuída | `./test_fila.sh` |

---

## Licença

Projeto académico — Redes de Computadores.
