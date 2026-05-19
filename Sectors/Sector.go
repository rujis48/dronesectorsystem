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

	servers := make([]raft.Server, len(peers)+1)
	servers[0] = raft.Server{
		ID:      raft.ServerID(id),
		Address: raft.ServerAddress(bindAddr),
	}

	for i, peer := range peers {
		parts := strings.Split(peer, ":")
		if len(parts) != 2 {
			log.Fatal("Invalid peer format")
		}
		peerID := parts[0]

		peerIPs, err := net.LookupIP(peerID)
		if err != nil {
			log.Fatalf("Falha ao resolver IP do peer %s: %v", peerID, err)
		}
		realPeerAddr := peerIPs[0].String() + ":" + parts[1]

		servers[i+1] = raft.Server{
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
								finishCmd := drones.Command{Op: "finish_fix", DroneID: d.ID}
								data, _ := json.Marshal(finishCmd)
								r.Apply(data, 10*time.Second)
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

			// CORREÇÃO: Sorteia qual setor da rede vai sofrer o problema (pode ser o 1, 2 ou 3)
			targetSector := allSectors[rand.Intn(len(allSectors))]

			log.Printf("[LÍDER] Problema %s GERADO GLOBALMENTE para o %s. Tempo estimado: %ds", severity, targetSector, durationSecs)

			// Enviamos o targetSector sorteado no comando Raft, em vez do ID local fixo
			cmd := drones.Command{
				Op:              "assign",
				SectorID:        targetSector, // <--- Agora o comando leva o setor sorteado!
				Severity:        severity,
				DurationSeconds: durationSecs,
			}
			data, _ := json.Marshal(cmd)
			future := r.Apply(data, 10*time.Second)
			if future.Error() != nil {
				log.Printf("Failed to apply command: %s", future.Error())
			} else {
				if response := future.Response(); response != nil {
					assigned, ok := response.([]int)
					if !ok || len(assigned) == 0 {
						log.Printf("[LÍDER] Falha ao alocar drones para o %s (Pool de drones vazio)", targetSector)
						continue
					}
					log.Printf("[LÍDER] %d drone(s) BLOQUEADO(S) com sucesso para o %s (Problema: %s)", len(assigned), targetSector, severity)
				}
			}
		}
	}()

	go func() {
		for {
			time.Sleep(10 * time.Second)
			log.Printf("Sector %s: Available drones locally or globally here: %d", id, fsm.GetAvailableCount())
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

	go func() {
		addr := "0.0.0.0:" + httpPort
		log.Printf("Starting HTTP status server on %s", addr)
		if err := http.ListenAndServe(addr, mux); err != nil {
			log.Printf("HTTP server error: %s", err)
		}
	}()

	select {}
}
