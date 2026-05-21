#!/bin/bash

clear
echo "=================================================="
echo "      CONFIGURADOR DE AMBIENTE DINÂMICO           "
echo "=================================================="

# 1. Definição da quantidade de setores
read -p "Quantos setores (nós) farão parte do cluster? (Ex: 3): " total_setores
if ! [[ "$total_setores" =~ ^[0-9]+$ ]] || [ "$total_setores" -le 0 ]; then
    echo "Erro: Quantidade inválida."
    exit 1
fi

# Como você deseja definir os IPs dos setores?
echo ""
echo "1) Digitar manualmente o IP de cada setor (multi-máquina)"
echo "2) Usar IPs automáticos (nomes Docker) - para ambiente local dockerizado"
read -p "Escolha uma opção (1 ou 2): " ip_choice

# Validar a escolha
if [[ ! "$ip_choice" =~ ^[1-2]$ ]]; then
    echo "Erro: Opção inválida."
    exit 1
fi

# 2. Coleta de IPs de todos os setores
declare -A sector_ips
echo ""
if [ "$ip_choice" -eq 1 ]; then
    echo "--- PASSO 1: Mapeamento de IPs (Manual) ---"
    echo "Para cada setor, digite o IP do computador onde ele vai rodar."
    echo "Setores no MESMO computador devem ter o MESMO IP."
    echo ""
    for ((i=1; i<=total_setores; i++)); do
        read -p "IP do computador onde o Sector $i vai rodar: " ip

        # Validação simples de IP
        if ! [[ "$ip" =~ ^[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}$ ]]; then
            echo "Erro: IP inválido ($ip)."
            exit 1
        fi
        sector_ips[$i]=$ip
    done
else
    echo "--- PASSO 1: Mapeamento de IPs (Automático - Local) ---"
    for ((i=1; i<=total_setores; i++)); do
        sector_ips[$i]="sector$i"
        echo "Setor $i: IP definido automaticamente como sector$i"
    done
fi

# 3. Identificação do que roda NESTA máquina
echo ""
echo "--- PASSO 2: Configuração Local ---"
echo "Quais setores você deseja que rodem NESTA máquina específica?"
echo "Instrução: Digite os números separados por espaço (Ex: '1' ou '1 2' ou '1 3 4')"
read -p "Setores para esta máquina: " setores_locais

# Validar se o usuário digitou pelo menos um setor válido
has_local=false
for s in $setores_locais; do
    if [[ "$s" =~ ^[0-9]+$ ]] && [ "$s" -le "$total_setores" ] && [ "$s" -gt 0 ]; then
        has_local=true
    fi
done

if [ "$has_local" = false ]; then
    echo "Erro: Você precisa selecionar pelo menos um setor válido para esta máquina."
    exit 1
fi

# 4. Construção da string de PEERS
# Regra: setores no mesmo computador comunicam via nome Docker (sector1, sector2)
# Setores em computadores diferentes comunicam via IP real
# Formato: sectorID=host:port
#
# Se ip_choice == 2 (automático), todos usam nomes Docker
# Se ip_choice == 1 (manual):
#   - Setares com o mesmo IP → usam nome Docker entre si
#   - Setores com IP diferente → usam IP real

peers_string=""
for ((i=1; i<=total_setores; i++)); do
    port=$((5000 + i))
    host_for_peer="${sector_ips[$i]}"

    if [ "$ip_choice" -eq 1 ]; then
        # Verifica se algum setor LOCAL tem o mesmo IP que este setor
        # Se sim, usa nome Docker; senão, usa IP real
        for local_s in $setores_locais; do
            if [[ "$local_s" =~ ^[0-9]+$ ]] && [ "${sector_ips[$local_s]}" == "${sector_ips[$i]}" ]; then
                host_for_peer="sector$i"
                break
            fi
        done
    fi

    peers_string+="sector${i}=${host_for_peer}:$port"
    if [ $i -lt $total_setores ]; then
        peers_string+=","
    fi
done

echo ""
echo "PEERS gerado: $peers_string"

# 5. Geração do docker-compose.yaml customizado
echo ""
echo "Gerando docker-compose.yaml..."

# Início do arquivo
cat << EOF > docker-compose.yaml
services:
EOF

# Adiciona apenas os serviços selecionados para esta máquina
for s in $setores_locais; do
    # Garante que é um número válido dentro do escopo
    if ! [[ "$s" =~ ^[0-9]+$ ]] || [ "$s" -gt "$total_setores" ] || [ "$s" -le 0 ]; then
        continue
    fi

    port_raft=$((5000 + s))
    port_http=$((7000 + s))

    # ADVERTISE_ADDR: usa o IP real da máquina (para que peers remotos alcancem este nó)
    # Só necessário em modo manual (multi-máquina)
    if [ "$ip_choice" -eq 1 ]; then
        advertise_line="      - ADVERTISE_ADDR=${sector_ips[$s]}:$port_raft"
    else
        advertise_line=""
    fi

    cat << EOF >> docker-compose.yaml
  sector$s:
    build:
      context: .
      dockerfile: Sectors/Dockerfile
    container_name: sector$s
    restart: always
    environment:
      - SECTOR_ID=sector$s
      - BIND_ADDR=0.0.0.0:$port_raft
      - PEERS=$peers_string
      - DATA_DIR=/tmp/raft-sector$s
      - HTTP_PORT=$port_http
${advertise_line}
    ports:
      - "$port_raft:$port_raft"
      - "$port_http:$port_http"
    volumes:
      - raft-data-$s:/tmp/raft-sector$s

EOF
done

# Adiciona a seção de volumes apenas para os setores locais
cat << EOF >> docker-compose.yaml
volumes:
EOF

for s in $setores_locais; do
    if ! [[ "$s" =~ ^[0-9]+$ ]] || [ "$s" -gt "$total_setores" ] || [ "$s" -le 0 ]; then
        continue
    fi
    echo "  raft-data-$s:" >> docker-compose.yaml
done

# 6. Finalização e limpeza do Docker
echo "Limpando volumes antigos locais para evitar conflitos..."
sudo docker compose down -v 2>/dev/null

echo ""
echo "=================================================="
echo "  Configuração concluída com sucesso!"
echo "=================================================="
echo ""
echo "  Arquivo: docker-compose.yaml"
echo "  Setores locais: $setores_locais"
echo "  PEERS: $peers_string"
echo ""
echo "  IMPORTANTE: Execute o mesmo configure.sh nas OUTRAS"
echo "  máquinas, escolhendo os setores correspondentes."
echo ""
echo "  Para iniciar: docker compose up --build"
echo "=================================================="
