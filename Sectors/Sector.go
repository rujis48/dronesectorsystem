package main

import (
	"encoding/json"
	"log"
	"math/rand"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	drones "dronesectorsystem/Drones"

	"github.com/hashicorp/raft"
	raftboltdb "github.com/hashicorp/raft-boltdb/v2"
)

func selectProblemSeverity() string {
	choice := rand.Intn(100)
	switch {
	case choice < 50:
		return "easy"
	case choice < 80:
		return "medium"
	default:
		return "hard"
	}
}

func computeFixDuration(severity string, dronesAssigned int) time.Duration {
	var minSec, maxSec int
	switch severity {
	case "easy":
		minSec, maxSec = 10, 15
	case "medium":
		minSec, maxSec = 18, 25
	case "hard":
		minSec, maxSec = 25, 35
	default:
		minSec, maxSec = 10, 15
	}
	base := time.Duration(rand.Intn(maxSec-minSec+1)+minSec) * time.Second
	if dronesAssigned <= 1 {
		return base
	}
	optimized := time.Duration(int64(base) / int64(dronesAssigned))
	if optimized < 5*time.Second {
		return 5 * time.Second
	}
	return optimized
}

func main() {
	id := os.Getenv("SECTOR_ID")
	if id == "" {
		log.Fatal("SECTOR_ID not set")
	}
	bindAddrEnv := os.Getenv("BIND_ADDR")
	if bindAddrEnv == "" {
		log.Fatal("BIND_ADDR not set")
	}
	dataDir := os.Getenv("DATA_DIR")
	if dataDir == "" {
		dataDir = "/tmp/raft-" + id
	}
	peersStr := os.Getenv("PEERS")
	if peersStr == "" {
		log.Fatal("PEERS not set")
	}
	peers := strings.Split(peersStr, ",")

	// --- RESOLUÇÃO DINÂMICA DE ENDEREÇO UNIVERSAL ---
	host, portPart, err := net.SplitHostPort(bindAddrEnv)
	if err != nil {
		log.Fatalf("Erro ao decodificar BIND_ADDR: %v", err)
	}

	if host == "0.0.0.0" {
		host = id
	}

	ips, err := net.LookupIP(host)
	if err != nil {
		log.Fatalf("Falha ao resolver IP para o host %s: %v", host, err)
	}
	realIP := ips[0].String()
	bindAddr := realIP + ":" + portPart

	log.Printf("[RAFT] Configurando nó %s com endereço anunciável: %s", id, bindAddr)
	// -------------------------------------------------

	initialCount := 5
	fsm := drones.NewFSM(initialCount)

	config := raft.DefaultConfig()
	config.LocalID = raft.ServerID(id)

	addr, err := net.ResolveTCPAddr("tcp", bindAddr)
	if err != nil {
		log.Fatal(err)
	}
	transport, err := raft.NewTCPTransport(bindAddr, addr, 3, 10*time.Second, os.Stderr)
	if err != nil {
		log.Fatal(err)
	}

	logStore, err := raftboltdb.NewBoltStore(dataDir + "/raft.db")
	if err != nil {
		log.Fatal(err)
	}

	stableStore, err := raftboltdb.NewBoltStore(dataDir + "/stable.db")
	if err != nil {
		log.Fatal(err)
	}

	snapshotStore, err := raft.NewFileSnapshotStore(dataDir, 2, os.Stderr)
	if err != nil {
		log.Fatal(err)
	}

	r, err := raft.NewRaft(config, fsm, logStore, stableStore, snapshotStore, transport)
	if err != nil {
		log.Fatal(err)
	}

	// Count how many peers are not self to size the array correctly
	peerCount := 0
	for _, peer := range peers {
		parts := strings.Split(peer, ":")
		if len(parts) != 2 {
			continue
		}
		peerID := parts[0]
		if peerID != id {
			peerCount++
		}
	}

	servers := make([]raft.Server, 1+peerCount)
	servers[0] = raft.Server{
		ID:      raft.ServerID(id),
		Address: raft.ServerAddress(bindAddr),
	}

	// Add peers, skipping self
	index := 0
	for _, peer := range peers {
		parts := strings.Split(peer, ":")
		if len(parts) != 2 {
			continue
		}
		peerID := parts[0]
		if peerID == id {
			continue // Skip self
		}

		peerIPs, err := net.LookupIP(peerID)
		if err != nil {
			log.Fatalf("Falha ao resolver IP do peer %s: %v", peerID, err)
		}
		realPeerAddr := peerIPs[0].String() + ":" + parts[1]

		index++
		servers[index] = raft.Server{
			ID:      raft.ServerID(peerID),
			Address: raft.ServerAddress(realPeerAddr),
		}
	}

	go func() {
		for {
			if r.State() == raft.Leader {
				log.Printf("Sector %s is leader", id)
				break
			}
			time.Sleep(1 * time.Second)
		}
	}()

	if id == "sector1" {
		future := r.BootstrapCluster(raft.Configuration{Servers: servers})
		if err := future.Error(); err != nil {
			log.Printf("Bootstrap error: %s", err)
		}
	}

	// Rotina do Líder: Monitoramento e liberação por expiração de tempo dos Drones
	go func() {
		for {
			time.Sleep(1 * time.Second)
			if r.State() == raft.Leader {
				cmd := drones.Command{Op: "get_drones"}
				data, _ := json.Marshal(cmd)
				future := r.Apply(data, 10*time.Second)
				if future.Error() == nil {
					if dronesList := future.Response(); dronesList != nil {
						ds := dronesList.([]drones.Drone)
						for _, d := range ds {
							if d.Status == "fixing" && !d.FinishAt.IsZero() && time.Now().After(d.FinishAt) {
								log.Printf("[LÍDER] Tempo esgotado! Liberando drone %d do Setor %s", d.ID, d.AssignedTo)

								// Testa falha simulada do drone (30% de chance)
								failureCmd := drones.Command{Op: "simulate_drone_failure", DroneID: d.ID}
								failureData, _ := json.Marshal(failureCmd)
								failureFuture := r.Apply(failureData, 10*time.Second)

								if failureFuture.Error() == nil {
									if failureResult := failureFuture.Response(); failureResult != nil {
										if failureMap, ok := failureResult.(map[string]interface{}); ok {
											if failed, ok := failureMap["failed"].(bool); ok && failed {
												log.Printf("[FAILURE] ✗ Drone %d FALHOU após o conserto!", d.ID)
												// Falha de drone é registrada, aguardando healthcheck
												continue
											}
										}
									}
								}

								// Drone bem-sucedido: Envia heartbeat
								heartbeatCmd := drones.Command{Op: "heartbeat_drone", DroneID: d.ID}
								heartbeatData, _ := json.Marshal(heartbeatCmd)
								r.Apply(heartbeatData, 10*time.Second)
								log.Printf("[HEARTBEAT] ✓ Drone %d respondeu (healthcheck OK)", d.ID)

								// Libera o drone normalmente
								finishCmd := drones.Command{Op: "finish_fix", DroneID: d.ID}
								finishData, _ := json.Marshal(finishCmd)
								r.Apply(finishData, 10*time.Second)

								// Tenta processar fila após liberar drone
								log.Printf("[LÍDER] Processando fila distribuída após liberação...")
								processQueueCmd := drones.Command{
									Op:              "process_queue",
									DurationSeconds: 0, // Será recalculado ao processar
								}
								queueData, _ := json.Marshal(processQueueCmd)
								r.Apply(queueData, 10*time.Second)
							}
						}
					}
				}
			}
		}
	}()

	// Rotina do Líder: Monitoramento contínuo da fila distribuída
	// Processa requisições enfileiradas periodicamente
	go func() {
		for {
			time.Sleep(5 * time.Second) // Processa fila a cada 5 segundos
			if r.State() == raft.Leader {
				queueSize := fsm.GetQueueSize()
				if queueSize > 0 {
					log.Printf("[LÍDER] Fila distribuída com %d requisição(ões). Tentando processar...", queueSize)

					processQueueCmd := drones.Command{
						Op:              "process_queue",
						DurationSeconds: 0,
					}
					data, _ := json.Marshal(processQueueCmd)
					future := r.Apply(data, 10*time.Second)

					if future.Error() == nil {
						if result := future.Response(); result != nil {
							if resultMap, ok := result.(map[string]interface{}); ok {
								processed := resultMap["processed"]
								remaining := resultMap["remaining_queue"]
								log.Printf("[FILA] Processamento: %v satisfeitas, %v restando na fila", processed, remaining)
							}
						}
					} else {
						log.Printf("[FILA] Erro ao processar: %v", future.Error())
					}
				}
			}
		}
	}()

	// Rotina do Líder: Healthcheck de Drones - Detecta e recupera drones mortos
	go func() {
		for {
			time.Sleep(7 * time.Second) // Verifica a cada 7 segundos
			if r.State() == raft.Leader {
				// Detecta drones mortos
				detectCmd := drones.Command{Op: "detect_dead_drones"}
				data, _ := json.Marshal(detectCmd)
				future := r.Apply(data, 10*time.Second)

				if future.Error() == nil {
					if result := future.Response(); result != nil {
						if resultMap, ok := result.(map[string]interface{}); ok {
							deadDronesIface := resultMap["dead_drones"]
							if deadDronesSlice, ok := deadDronesIface.([]int); ok && len(deadDronesSlice) > 0 {
								for _, droneID := range deadDronesSlice {
									// Obtém informações do drone morto
									getDronesCmd := drones.Command{Op: "get_drones"}
									getDronesData, _ := json.Marshal(getDronesCmd)
									getDronesFuture := r.Apply(getDronesData, 10*time.Second)
									if getDronesFuture.Error() == nil {
										if dronesList := getDronesFuture.Response(); dronesList != nil {
											dronesList := dronesList.([]drones.Drone)
											var droneSetor string
											for _, d := range dronesList {
												if d.ID == droneID {
													droneSetor = d.AssignedTo
													break
												}
											}
											// Cria novo drone para substituição
											createCmd := drones.Command{
												Op:       "create_drone",
												SectorID: droneSetor,
											}
											createData, _ := json.Marshal(createCmd)
											r.Apply(createData, 10*time.Second)
										}
									}
								}
							}
						}
					}
				}
			}
		}
	}()

	// Causador de problemas (O Líder atua como gerador global de demandas para todo o ecossistema)
	go func() {
		rand.Seed(time.Now().UnixNano())

		// Lista de setores conhecidos pelo ecossistema para sorteio distribuído
		allSectors := []string{"sector1", "sector2", "sector3"}

		for {
			interval := time.Duration(rand.Intn(10)+5) * time.Second
			time.Sleep(interval)

			// Se este nó não for o líder, ele permanece passivo
			if r.State() != raft.Leader {
				continue
			}

			severity := selectProblemSeverity()

			estimatedDrones := 1
			switch severity {
			case "medium":
				estimatedDrones = 2
			case "hard":
				estimatedDrones = 3
			}

			fixTime := computeFixDuration(severity, estimatedDrones)
			durationSecs := int(fixTime.Seconds())

			// Sorteia qual setor da rede vai sofrer o problema (pode ser o 1, 2 ou 3)
			targetSector := allSectors[rand.Intn(len(allSectors))]

			log.Printf("[LÍDER] Problema %s GERADO para %s. Drones necessários: %d. Duração: %ds", severity, targetSector, estimatedDrones, durationSecs)

			// Comando com timeout melhorado (20 segundos para aplicação)
			cmd := drones.Command{
				Op:              "assign_with_fallback",
				SectorID:        targetSector,
				Severity:        severity,
				DurationSeconds: durationSecs,
			}
			data, _ := json.Marshal(cmd)
			future := r.Apply(data, 20*time.Second)

			// Tratamento robusto de erros e timeouts
			if future.Error() != nil {
				log.Printf("[LÍDER] ✗ Erro ao aplicar comando para %s: %v", targetSector, future.Error())
				continue
			}

			response := future.Response()
			if response == nil {
				log.Printf("[LÍDER] ✗ Resposta nula ao alocar drones para %s", targetSector)
				continue
			}

			assigned, ok := response.([]int)
			if !ok {
				log.Printf("[LÍDER] ✗ Tipo de resposta inválido para %s", targetSector)
				continue
			}

			if len(assigned) == 0 {
				log.Printf("[LÍDER] ✗ Sem drones disponíveis para alocar em %s", targetSector)
				continue
			}

			if len(assigned) < estimatedDrones {
				log.Printf("[LÍDER] ⚠ Alocação parcial para %s: %d de %d drones solicitados", targetSector, len(assigned), estimatedDrones)
			} else {
				log.Printf("[LÍDER] ✓ Alocação bem-sucedida: %d drone(s) para %s (severidade=%s)", len(assigned), targetSector, severity)
			}
		}
	}()

	go func() {
		for {
			time.Sleep(10 * time.Second)
			log.Printf("[STATUS] Sector %s: %d drones disponíveis", id, fsm.GetAvailableCount())
		}
	}()

	httpPort := os.Getenv("HTTP_PORT")
	if httpPort == "" {
		switch id {
		case "sector1":
			httpPort = "7001"
		case "sector2":
			httpPort = "7002"
		case "sector3":
			httpPort = "7003"
		default:
			httpPort = "7000"
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/status", func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")

		ds := fsm.GetDrones()
		if ds == nil {
			ds = []drones.Drone{}
		}

		status := map[string]interface{}{
			"sector_id": id,
			"is_leader": r.State() == raft.Leader,
			"drones":    ds,
		}
		_ = json.NewEncoder(w).Encode(status)
	})

	// endpoint de 'Health Check' com confirmação de recebimento
	mux.HandleFunc("/confirm", func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")

		// Verifica se o Raft está em estado saudável (não Shutdown)
		// States válidos: Follower, Candidate, Leader
		currentState := r.State()
		isHealthy := currentState != raft.Shutdown

		confirmResp := map[string]interface{}{
			"confirmed":  isHealthy,
			"sector_id":  id,
			"is_leader":  currentState == raft.Leader,
			"timestamp":  time.Now().Format("2006-01-02T15:04:05Z07:00"),
			"raft_state": currentState.String(),
		}

		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(confirmResp)
	})

	// Endpoint para consultar a fila distribuída
	mux.HandleFunc("/queue", func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")

		queue := fsm.GetQueue()
		if queue == nil {
			queue = make([]drones.PendingRequest, 0)
		}

		queueResp := map[string]interface{}{
			"sector_id":  id,
			"queue_size": len(queue),
			"is_leader":  r.State() == raft.Leader,
			"queue":      queue,
		}
		_ = json.NewEncoder(w).Encode(queueResp)
	})

	// Endpoint com estatísticas da fila
	mux.HandleFunc("/queue/status", func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")

		queue := fsm.GetQueue()
		drones := fsm.GetDrones()

		availableCount := 0
		for _, d := range drones {
			if d.Status == "available" {
				availableCount++
			}
		}

		var oldestReqAge time.Duration
		if len(queue) > 0 {
			oldestReqAge = time.Since(queue[0].QueuedAt)
		}

		queueStatus := map[string]interface{}{
			"sector_id":              id,
			"queue_size":             len(queue),
			"drones_available":       availableCount,
			"drones_total":           len(drones),
			"oldest_request_age_sec": int(oldestReqAge.Seconds()),
			"is_leader":              r.State() == raft.Leader,
		}
		_ = json.NewEncoder(w).Encode(queueStatus)
	})

	// Endpoint para visualizar status de healthcheck dos drones
	mux.HandleFunc("/drones/health", func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")

		dronesList := fsm.GetDrones()
		if dronesList == nil {
			dronesList = make([]drones.Drone, 0)
		}

		type DroneHealthInfo struct {
			ID                   int       `json:"id"`
			Status               string    `json:"status"`
			Health               string    `json:"health"`
			AssignedTo           string    `json:"assigned_to"`
			LastHeartbeat        time.Time `json:"last_heartbeat"`
			ConsecutiveFailures  int       `json:"consecutive_failures"`
			ConsecutiveSuccesses int       `json:"consecutive_successes"`
			TimeSinceHeartbeat   int       `json:"time_since_heartbeat_sec"`
		}

		healthInfo := make([]DroneHealthInfo, len(dronesList))
		now := time.Now()
		for i, d := range dronesList {
			timeSince := int(now.Sub(d.LastHeartbeat).Seconds())
			healthInfo[i] = DroneHealthInfo{
				ID:                   d.ID,
				Status:               d.Status,
				Health:               d.Health,
				AssignedTo:           d.AssignedTo,
				LastHeartbeat:        d.LastHeartbeat,
				ConsecutiveFailures:  d.ConsecutiveFailures,
				ConsecutiveSuccesses: d.ConsecutiveSuccesses,
				TimeSinceHeartbeat:   timeSince,
			}
		}

		healthResp := map[string]interface{}{
			"sector_id":     id,
			"is_leader":     r.State() == raft.Leader,
			"timestamp":     time.Now().Format("2006-01-02T15:04:05Z07:00"),
			"drones_health": healthInfo,
		}
		_ = json.NewEncoder(w).Encode(healthResp)
	})

	go func() {
		addr := "0.0.0.0:" + httpPort
		log.Printf("Starting HTTP status server on %s", addr)
		if err := http.ListenAndServe(addr, mux); err != nil {
			log.Printf("HTTP server error: %s", err)
		}
	}()

	select {}
}
