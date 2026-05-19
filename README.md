## Guia de Comandos Principais

Esta seção compila os comandos essenciais para gerenciar, monitorar e testar a tolerância a falhas do ecossistema de drones.

### 1. Operação em Máquina Única (Docker Compose)

Use estes comandos no diretório raiz do projeto para controlar o cluster simulado localmente:

* **Subir o cluster do zero (Limpando caches e compilando alterações):**
    ```bash
    sudo docker compose up --build
    ```
* **Derrubar o cluster removendo os volumes (Resetar o estado do banco Raft):**
    ```bash
    sudo docker compose down -v
    ```
* **Visualizar os logs de apenas um setor específico (Em tempo real):**
    ```bash
    sudo docker logs -f sector1
    ```

---

### 2. Operação em Múltiplos Computadores (Rede Física)

Caso opte por rodar o binário compilado nativamente em máquinas separadas no laboratório, utilize os comandos na seguinte ordem:

* **Compilar o executável em cada máquina:**
    ```bash
    go build -o sector ./Sectors/Sector.go
    ```
* **Dar permissão de execução ao binário gerado (Caso necessário no Linux):**
    ```bash
    chmod +x sector
    ```
* **Iniciar o programa (Após exportar as variáveis de ambiente descritas no guia de implantação):**
    ```bash
    ./sector
    ```

---

### 3. Interface Gráfica e Monitoramento

Para acompanhar a alocação de drones, o status do cluster e quem assumiu o papel de Líder:

* **Atualizar as dependências locais antes de rodar a interface:**
    ```bash
    go mod tidy
    ```
* **Iniciar o painel de monitoramento no terminal:**
    ```bash
    go run monitor.go
    ```
* **Consultar o estado bruto (JSON) de um nó via requisição HTTP direta:**
    ```bash
    curl http://localhost:7001/status
    ```
    *(Substitua a porta pelo o equivalente dos outros setores para a realização das consultas).*

---

### 4. Simulação de Cenários e Testes de Resiliência (Chaos Engineering)

Para validar o funcionamento do algoritmo de consenso Raft e os tempos de timeout de segurança, abra um terminal paralelo com o Docker ativo e execute:

* **Simular a queda de um Setor (Derrubar Seguidor ou Líder):**
    ```bash
    sudo docker stop sector2
    ```
    *(Observe no monitor.go a transição de estado e o disparo de uma nova eleição automática se o líder cair).*

* **Recuperar o Setor caído (Sincronização forçada de Logs):**
    ```bash
    sudo docker start sector2
    ```
    *(O nó voltará ao estado Online, identificará o líder atual e sincronizará sua máquina de estados automaticamente).*

* **Simular perda de maioria (Falha Crítica):**
    Derrube dois setores simultaneamente:
    ```bash
    sudo docker stop sector1 sector2
    ```
    *(O setor restante deve perder o cargo de líder e congelar a aplicação de novos comandos, protegendo o sistema contra o problema de Split-Brain).*