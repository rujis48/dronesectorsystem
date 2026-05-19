#!/bin/bash

clear
echo "=================================================="
echo "      CONFIGURADOR DE AMBIENTE: DRONE SECTOR       "
echo "=================================================="
echo "1) Rodar TUDO LOCAL (Nesta mesma máquina)"
echo "2) Rodar DISTRIBUÍDO (Múltiplos computadores físicos)"
echo "=================================================="
read -p "Escolha uma opção (1 ou 2): " opcao

if [ "$opcao" == "1" ]; then
    echo "Configurando para ambiente LOCAL..."
    
    # Substitui as marcações pelos hostnames internos do Docker
    sed -e 's/s1_bind/sector1:5001/g' \
        -e 's/s1_peers/sector2:5002,sector3:5003/g' \
        -e 's/s2_bind/sector2:5002/g' \
        -e 's/s2_peers/sector1:5001,sector3:5003/g' \
        -e 's/s3_bind/sector3:5003/g' \
        -e 's/s3_peers/sector1:5001,sector2:5002/g' \
        docker-compose.template.yaml > docker-compose.yaml

    echo "Limpando volumes antigos para evitar conflitos..."
    sudo docker compose down -v
    echo "✓ Pronto! Agora basta rodar: sudo docker compose up --build"

elif [ "$opcao" == "2" ]; then
    echo "Configurando para ambiente DISTRIBUÍDO..."
    echo ""
    echo "PASSO 1: Informe os IPs dos três computadores"
    read -p "Digite o IP do computador onde Sector 1 rodará: " ip1
    read -p "Digite o IP do computador onde Sector 2 rodará: " ip2
    read -p "Digite o IP do computador onde Sector 3 rodará: " ip3
    
    echo ""
    echo "PASSO 2: Selecione qual setor roda NESTE computador"
    read -p "Qual setor você quer rodar aqui? (1, 2 ou 3): " sector_choice

    # Validação de entrada
    if ! [[ "$sector_choice" =~ ^[1-3]$ ]]; then
        echo "❌ Erro: Escolha um setor válido (1, 2 ou 3)"
        exit 1
    fi

    # Validação de IPs
    echo ""
    echo "Validando IPs..."
    for ip in "$ip1" "$ip2" "$ip3"; do
        if ! [[ "$ip" =~ ^[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}$ ]]; then
            echo "❌ Erro: IP inválido - $ip"
            exit 1
        fi
    done
    echo "IPs validados"

    # Cria arquivo temporário
    temp_file=$(mktemp)
    
    # Substitui TODOS os placeholders pelos valores reais
    # IMPORTANTE: Usa 0.0.0.0 para BIND_ADDR (container liga em todas as interfaces)
    sed -e "s/s1_bind/0.0.0.0:5001/g" \
        -e "s/s1_peers/$ip1:5001,$ip2:5002,$ip3:5003/g" \
        -e "s/s2_bind/0.0.0.0:5002/g" \
        -e "s/s2_peers/$ip1:5001,$ip2:5002,$ip3:5003/g" \
        -e "s/s3_bind/0.0.0.0:5003/g" \
        -e "s/s3_peers/$ip1:5001,$ip2:5002,$ip3:5003/g" \
        docker-compose.template.yaml > "$temp_file"

    # Automaticamente comenta os setores que NÃO devem rodar neste computador
    case $sector_choice in
        1)
            # Comenta sector2 e sector3
            sed -i '/^  sector2:$/,/^  [^ ]/{ /^  [^ ]/!s/^/#/; }' "$temp_file"
            sed -i '/^  sector3:$/,/^volumes:$/{ /^volumes:$/!s/^/#/; }' "$temp_file"
            echo "Configurado: Sector 1 será executado neste computador ($ip1:5001)"
            ;;
        2)
            # Comenta sector1 e sector3
            sed -i '/^  sector1:$/,/^  sector2:$/{ /^  sector2:$/!s/^/#/; }' "$temp_file"
            sed -i '/^  sector3:$/,/^volumes:$/{ /^volumes:$/!s/^/#/; }' "$temp_file"
            echo "Configurado: Sector 2 será executado neste computador ($ip2:5002)"
            ;;
        3)
            # Comenta sector1 e sector2
            sed -i '/^  sector1:$/,/^  sector2:$/{ /^  sector2:$/!s/^/#/; }' "$temp_file"
            sed -i '/^  sector2:$/,/^  sector3:$/{ /^  sector3:$/!s/^/#/; }' "$temp_file"
            echo "Configurado: Sector 3 será executado neste computador ($ip3:5003)"
            ;;
    esac

    # Move arquivo gerado para o destino final
    mv "$temp_file" docker-compose.yaml

    echo ""
    echo "Ambiente distribuído gerado com sucesso!"
    echo "Limpando volumes antigos para evitar conflitos..."
    sudo docker compose down -v
    echo ""
    echo "Configuração completa! Agora basta rodar:"
    echo "sudo docker compose up --build"
    echo ""
    echo "IMPORTANTE: Execute este script em cada computador da rede com os MESMOS IPs!"

else
    echo "Opção inválida."
    exit 1
fi