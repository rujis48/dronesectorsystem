package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
)

// Estruturas correspondentes ao JSON do sistema de setores
type Drone struct {
	ID         int    `json:"id"`
	AssignedTo string `json:"assigned_to"`
	Status     string `json:"status"`
}

type StatusResponse struct {
	SectorID string  `json:"sector_id"`
	IsLeader bool    `json:"is_leader"`
	Drones   []Drone `json:"drones"`
}

type ConfirmResponse struct {
	Confirmed bool   `json:"confirmed"`
	SectorID  string `json:"sector_id"`
	Timestamp string `json:"timestamp"`
}

type SectorState struct {
	Online   bool
	IsLeader bool
}

// fetchSectorStatus faz retransmissão automática com backoff exponencial
// Tenta até 3 vezes com delay incrementando entre tentativas
func fetchSectorStatus(url string) (*StatusResponse, error) {
	maxRetries := 3
	timeout := 800 * time.Millisecond

	for attempt := 1; attempt <= maxRetries; attempt++ {
		client := http.Client{Timeout: timeout}
		resp, err := client.Get(url)

		// Sucesso
		if err == nil && resp.StatusCode == 200 {
			var sr StatusResponse
			decodeErr := json.NewDecoder(resp.Body).Decode(&sr)
			resp.Body.Close()

			if decodeErr == nil {
				return &sr, nil
			}
			// Se falhar ao decodificar, retenta
		} else if resp != nil {
			resp.Body.Close()
		}

		// Se for a última tentativa, retorna erro
		if attempt == maxRetries {
			return nil, fmt.Errorf("falha após %d tentativas para %s", maxRetries, url)
		}

		// Backoff exponencial: 50ms, 150ms, 300ms
		backoff := time.Duration(50*attempt) * time.Millisecond
		time.Sleep(backoff)
	}

	return nil, fmt.Errorf("falha ao conectar em %s", url)
}

// confirmSectorHealth envia um health check com confirmação de recebimento
func confirmSectorHealth(url string) bool {
	confirmURL := strings.Replace(url, "/status", "/confirm", 1)
	client := http.Client{Timeout: 500 * time.Millisecond}
	resp, err := client.Get(confirmURL)

	if err != nil {
		return false
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return false
	}

	var cr ConfirmResponse
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		return false
	}

	return cr.Confirmed
}

func main() {
	// Endpoints HTTP dos seus containers Docker
	endpoints := map[string]string{
		"sector1": "http://localhost:7001/status",
		"sector2": "http://localhost:7002/status",
		"sector3": "http://localhost:7003/status",
	}

	for {
		// Comando ANSI para limpar a tela do terminal e resetar o cursor
		fmt.Print("\033[H\033[2J")

		fmt.Println("====================================================")
		fmt.Println("         MONITOR EM TEMPO REAL: DRONE SECTORS       ")
		fmt.Println("====================================================")
		fmt.Printf("Última atualização: %s\n\n", time.Now().Format("15:04:05"))

		sectorsInfo := make(map[string]SectorState)
		var globalDrones []Drone
		dataFetched := false

		// Coleta os dados de todos os setores COM RETRANSMISSÃO
		for sectorID, url := range endpoints {
			sr, err := fetchSectorStatus(url)

			if err != nil {
				sectorsInfo[sectorID] = SectorState{Online: false, IsLeader: false}
				log.Printf("[MONITOR] Setor %s offline: %v", sectorID, err)
				continue
			}

			// Confirma que setor respondeu corretamente (health check)
			isHealthy := confirmSectorHealth(url)
			if !isHealthy {
				log.Printf("[MONITOR] Health check falhou para %s", sectorID)
			}

			sectorsInfo[sectorID] = SectorState{Online: true, IsLeader: sr.IsLeader}
			if !dataFetched {
				globalDrones = sr.Drones
				dataFetched = true
			}
		}

		// Renderiza a caixa de cada um dos 3 setores
		for _, sectorNum := range []string{"1", "2", "3"} {
			sectorID := "sector" + sectorNum
			info := sectorsInfo[sectorID]

			// Define a tag de status do setor
			statusTag := "\033[31mOFFLINE\033[0m"
			if info.Online {
				statusTag = "\033[32mONLINE\033[0m"
				if info.IsLeader {
					statusTag = "\033[33mLÍDER ★\033[0m"
				}
			}

			// Desenha o topo da caixa do setor
			fmt.Printf("┌────────────────────────────────────────────────────┐\n")
			fmt.Printf("  SETOR %s [%s]\n", sectorNum, statusTag)
			fmt.Printf("├────────────────────────────────────────────────────┤\n")

			if !info.Online && !dataFetched {
				fmt.Println("  │ [Sem dados: Setor inacessível e cluster offline]")
			} else {
				// Separa e quantifica os drones deste setor específico
				var availableCount, fixingCount int
				var visualAsterisks []string

				for _, d := range globalDrones {
					if d.AssignedTo == sectorID {
						if d.Status == "available" {
							availableCount++
							// Asterisco Verde para drone disponível/estacionado
							visualAsterisks = append(visualAsterisks, "\033[32m*\033[0m")
						} else {
							fixingCount++
							// Asterisco Vermelho para drone trabalhando em problema
							visualAsterisks = append(visualAsterisks, "\033[31m*\033[0m")
						}
					}
				}

				// Monta a linha interna com os drones do setor
				dronesLine := strings.Join(visualAsterisks, " ")
				if dronesLine == "" {
					dronesLine = "receptáculo vazio"
				}

				fmt.Printf("  │ Drones no Setor: [ %s ]\n", dronesLine)
				fmt.Printf("  │ Estado atual: %d ociosos / %d consertando\n", availableCount, fixingCount)
			}
			fmt.Printf("└────────────────────────────────────────────────────┘\n\n")
		}

		// Painel de Legendas
		fmt.Println("Legenda:")
		fmt.Println("  \033[32m*\033[0m  = Drone Disponível (Estacionado no setor)")
		fmt.Println("  \033[31m*\033[0m  = Drone Ativo (Consertando problema neste setor)")
		fmt.Println("\nPressione Ctrl+C para encerrar o monitor.")

		// Atualiza o terminal a cada 1 segundo
		time.Sleep(1 * time.Second)
	}
}
