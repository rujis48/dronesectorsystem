package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
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
	Online        bool
	IsLeader      bool
	LastSeen      time.Time
	MissedCycles  int
}

// discoverSectors escaneia um intervalo de portas HTTP e retorna os setores encontrados.
// Executa as requisições em paralelo para velocidade.
func discoverSectors(host string, portStart, portEnd int) map[string]string {
	type result struct {
		sectorID string
		url      string
	}

	resultsCh := make(chan result, portEnd-portStart+1)
	var wg sync.WaitGroup

	for port := portStart; port <= portEnd; port++ {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			url := fmt.Sprintf("http://%s:%d/status", host, p)
			client := http.Client{Timeout: 300 * time.Millisecond}
			resp, err := client.Get(url)
			if err != nil {
				return
			}
			defer resp.Body.Close()

			if resp.StatusCode != 200 {
				return
			}

			var sr StatusResponse
			if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
				return
			}

			if sr.SectorID != "" {
				resultsCh <- result{sectorID: sr.SectorID, url: url}
			}
		}(port)
	}

	wg.Wait()
	close(resultsCh)

	endpoints := make(map[string]string)
	for r := range resultsCh {
		endpoints[r.sectorID] = r.url
	}
	return endpoints
}

// fetchSectorStatus faz retransmissão automática com backoff exponencial
func fetchSectorStatus(url string) (*StatusResponse, error) {
	maxRetries := 3
	timeout := 800 * time.Millisecond

	for attempt := 1; attempt <= maxRetries; attempt++ {
		client := http.Client{Timeout: timeout}
		resp, err := client.Get(url)

		if err == nil && resp.StatusCode == 200 {
			var sr StatusResponse
			decodeErr := json.NewDecoder(resp.Body).Decode(&sr)
			resp.Body.Close()
			if decodeErr == nil {
				return &sr, nil
			}
		} else if resp != nil {
			resp.Body.Close()
		}

		if attempt == maxRetries {
			return nil, fmt.Errorf("falha após %d tentativas para %s", maxRetries, url)
		}

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
	// Configuração via variáveis de ambiente
	host := os.Getenv("MONITOR_HOST")
	if host == "" {
		host = "localhost"
	}

	portStartStr := os.Getenv("MONITOR_PORT_START")
	portStart := 7001
	if portStartStr != "" {
		if v, err := strconv.Atoi(portStartStr); err == nil {
			portStart = v
		}
	}

	portEndStr := os.Getenv("MONITOR_PORT_END")
	portEnd := 7010
	if portEndStr != "" {
		if v, err := strconv.Atoi(portEndStr); err == nil {
			portEnd = v
		}
	}

	// Mapa persistente de setores conhecidos (sobrevive entre ciclos)
	knownSectors := make(map[string]*SectorState)
	const maxMissedCycles = 5 // ciclos antes de remover setor offline

	for {
		// Descobre setores ativos escaneando o intervalo de portas
		discovered := discoverSectors(host, portStart, portEnd)

		// Atualiza setores conhecidos: marca encontrados como online,
		// incrementa contador de falhos para os não encontrados
		for sectorID := range knownSectors {
			if _, found := discovered[sectorID]; !found {
				knownSectors[sectorID].Online = false
				knownSectors[sectorID].MissedCycles++
			}
		}

		for sectorID, url := range discovered {
			if existing, ok := knownSectors[sectorID]; ok {
				existing.Online = true
				existing.LastSeen = time.Now()
				existing.MissedCycles = 0
				// Atualiza URL caso a porta tenha mudado
				_ = url
			} else {
				knownSectors[sectorID] = &SectorState{
					Online:   true,
					LastSeen: time.Now(),
				}
			}
		}

		// Remove setores que excederam o limite de ciclos sem resposta
		for sectorID, state := range knownSectors {
			if !state.Online && state.MissedCycles > maxMissedCycles {
				missed := state.MissedCycles
				delete(knownSectors, sectorID)
				log.Printf("[MONITOR] Setor %s removido (offline há %d ciclos)", sectorID, missed)
			}
		}

		// Coleta dados dos setores online
		sectorsInfo := make(map[string]SectorState)
		var globalDrones []Drone
		dronesByID := make(map[int]Drone)

		for sectorID, url := range discovered {
			sr, err := fetchSectorStatus(url)
			if err != nil {
				sectorsInfo[sectorID] = SectorState{Online: false, IsLeader: false}
				log.Printf("[MONITOR] Setor %s offline: %v", sectorID, err)
				continue
			}

			isHealthy := confirmSectorHealth(url)
			if !isHealthy {
				log.Printf("[MONITOR] Health check falhou para %s", sectorID)
			}

			sectorsInfo[sectorID] = SectorState{Online: true, IsLeader: sr.IsLeader}

			// Coleta drones de todos os setores, deduplicando por ID
			for _, d := range sr.Drones {
				if _, exists := dronesByID[d.ID]; !exists {
					dronesByID[d.ID] = d
				}
			}
		}

		// Adiciona setores conhecidos que não foram encontrados (offline)
		for sectorID := range knownSectors {
			if _, inDiscovered := discovered[sectorID]; !inDiscovered {
				sectorsInfo[sectorID] = SectorState{Online: false, IsLeader: false}
			}
		}

		// Converte mapa de drones para slice
		for _, d := range dronesByID {
			globalDrones = append(globalDrones, d)
		}

		// Ordena setores por ID para exibição consistente
		sectorIDs := make([]string, 0, len(sectorsInfo))
		for id := range sectorsInfo {
			sectorIDs = append(sectorIDs, id)
		}
		sort.Strings(sectorIDs)

		// Limpa a tela
		fmt.Print("\033[H\033[2J")

		fmt.Println("====================================================")
		fmt.Println("         MONITOR EM TEMPO REAL: DRONE SECTORS       ")
		fmt.Println("====================================================")
		fmt.Printf("Última atualização: %s\n", time.Now().Format("15:04:05"))
		fmt.Printf("Setores descobertos: %d | Intervalo de portas: %d-%d\n\n", len(discovered), portStart, portEnd)

		dataFetched := len(globalDrones) > 0

		// Renderiza a caixa de cada setor
		for _, sectorID := range sectorIDs {
			info := sectorsInfo[sectorID]

			statusTag := "\033[31mOFFLINE\033[0m"
			if info.Online {
				statusTag = "\033[32mONLINE\033[0m"
				if info.IsLeader {
					statusTag = "\033[33mLÍDER ★\033[0m"
				}
			}

			// Extrai o número do setor para exibição
			displayName := strings.ToUpper(sectorID)

			fmt.Printf("┌────────────────────────────────────────────────────┐\n")
			fmt.Printf("  %s [%s]\n", displayName, statusTag)
			fmt.Printf("├────────────────────────────────────────────────────┤\n")

			if !info.Online && !dataFetched {
				fmt.Println("  │ [Sem dados: Setor inacessível e cluster offline]")
			} else {
				var availableCount, fixingCount int
				var visualAsterisks []string

				for _, d := range globalDrones {
					if d.AssignedTo == sectorID {
						if d.Status == "available" {
							availableCount++
							visualAsterisks = append(visualAsterisks, "\033[32m*\033[0m")
						} else {
							fixingCount++
							visualAsterisks = append(visualAsterisks, "\033[31m*\033[0m")
						}
					}
				}

				dronesLine := strings.Join(visualAsterisks, " ")
				if dronesLine == "" {
					dronesLine = "receptáculo vazio"
				}

				fmt.Printf("  │ Drones no Setor: [ %s ]\n", dronesLine)
				fmt.Printf("  │ Estado atual: %d ociosos / %d consertando\n", availableCount, fixingCount)
			}
			fmt.Printf("└────────────────────────────────────────────────────┘\n\n")
		}

		// Se nenhum setor foi encontrado
		if len(sectorIDs) == 0 {
			fmt.Println("  Nenhum setor encontrado no intervalo de portas configurado.")
			fmt.Printf("  Verificando portas %d a %d em %s...\n\n", portStart, portEnd, host)
		}

		// Painel de Legendas
		fmt.Println("Legenda:")
		fmt.Println("  \033[32m*\033[0m  = Drone Disponível (Estacionado no setor)")
		fmt.Println("  \033[31m*\033[0m  = Drone Ativo (Consertando problema neste setor)")
		fmt.Println("\nConfiguração: MONITOR_HOST, MONITOR_PORT_START, MONITOR_PORT_END")
		fmt.Println("Pressione Ctrl+C para encerrar o monitor.")

		time.Sleep(1 * time.Second)
	}
}
