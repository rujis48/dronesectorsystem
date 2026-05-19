#!/bin/bash

# Script de Teste: Fila Distribuída
# Este script envia múltiplas requisições para saturar o pool de drones
# e demonstra o funcionamento da fila distribuída com replanejamento automático

set -e

# Cores para output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Configuração
SECTOR_URL="http://localhost:7001"
MONITOR_INTERVAL=5

echo -e "${BLUE}========================================${NC}"
echo -e "${BLUE}  TESTE: Fila Distribuída${NC}"
echo -e "${BLUE}========================================${NC}\n"

# Função para enviar requisição
send_request() {
    local severity=$1
    local sector=$2
    local request_id="${sector}-$(date +%s%N)"
    
    echo -e "${YELLOW}[REQUISIÇÃO] Enviando ${severity} para ${sector}...${NC}"
    
    curl -s -X POST "${SECTOR_URL}/request" \
        -H "Content-Type: application/json" \
        -d "{
            \"severity\": \"${severity}\",
            \"sector_id\": \"${sector}\",
            \"request_id\": \"${request_id}\"
        }" \
        -w "\n" | jq . 2>/dev/null || echo "Erro ao enviar requisição"
}

# Função para verificar fila
check_queue() {
    echo -e "\n${BLUE}--- Status da Fila ---${NC}"
    curl -s "${SECTOR_URL}/queue/status" | jq '{
        queue_size: .queue_size,
        drones_available: .drones_available,
        drones_total: .drones_total,
        oldest_request_age_sec: .oldest_request_age_sec,
        is_leader: .is_leader
    }'
}

# Função para listar requisições enfileiradas
list_queue() {
    echo -e "\n${BLUE}--- Requisições Enfileiradas ---${NC}"
    curl -s "${SECTOR_URL}/queue" | jq '.queue | length as $count | 
        if $count == 0 then 
            "✓ Fila vazia!" 
        else 
            "📋 \($count) requisição(ões):\n" + 
            (.[] | "\(.id): \(.sector_id) (\(.severity))")
        end'
}

# Fase 1: Verificar estado inicial
echo -e "${GREEN}[Fase 1] Estado Inicial${NC}"
check_queue
list_queue

# Fase 2: Enviar requisições para saturar (5 drones no pool)
echo -e "\n${GREEN}[Fase 2] Saturando o Pool (Enviando 10 requisições 'hard')${NC}"
echo -e "${YELLOW}⚠ Aguarde, envio em progresso...${NC}\n"

for i in {1..10}; do
    send_request "hard" "sector$((($i % 3) + 1))" &
    sleep 0.2 # Small delay between sends
done

wait # Aguarda todas as requisições serem enviadas

# Fase 3: Verificar fila saturada
echo -e "\n${GREEN}[Fase 3] Verificando Fila Saturada (em ~5 segundos)${NC}"
sleep 6

check_queue
list_queue

# Fase 4: Monitorar processamento
echo -e "\n${GREEN}[Fase 4] Monitorando Processamento da Fila...${NC}"
echo -e "${YELLOW}(Verificando a cada ${MONITOR_INTERVAL}s por 60s)${NC}\n"

for i in {1..12}; do
    echo -e "\n${BLUE}--- Check #${i} (${((i * MONITOR_INTERVAL))}s) ---${NC}"
    check_queue
    echo -e "${BLUE}Drones:${NC}"
    curl -s "${SECTOR_URL}/status" | jq '.drones | group_by(.status) | map({status: .[0].status, count: length}) | sort_by(.status)'
    
    # Se fila vazia, pare de monitorar
    QUEUE_SIZE=$(curl -s "${SECTOR_URL}/queue/status" | jq '.queue_size')
    if [ "$QUEUE_SIZE" -eq 0 ]; then
        echo -e "\n${GREEN}✓ Fila completamente processada!${NC}"
        break
    fi
    
    sleep $MONITOR_INTERVAL
done

# Fase 5: Relatório Final
echo -e "\n${BLUE}========================================${NC}"
echo -e "${GREEN}[Fase 5] Relatório Final${NC}"
echo -e "${BLUE}========================================${NC}"

FINAL_QUEUE=$(curl -s "${SECTOR_URL}/queue/status" | jq '{
    queue_size: .queue_size,
    drones_available: .drones_available,
    drones_total: .drones_total
}')

echo "$FINAL_QUEUE"

echo -e "\n${GREEN}✓ Teste concluído!${NC}"
echo -e "${BLUE}========================================${NC}"
