package drones

import (
	"encoding/json"
	"io"
	"log"
	"math/rand"
	"sync"
	"time"

	"github.com/hashicorp/raft"
)

// Estrutura do Drone
type Drone struct {
	ID         int       `json:"id"`
	AssignedTo string    `json:"assigned_to"` // Sempre conterá o ID de um setor e nunca vai ser vazio
	Status     string    `json:"status"`      // "available", "fixing", "dead"
	FinishAt   time.Time `json:"finish_at"`   // Momento exato em que o drone deve ser liberado

	// Monitoramento dos drones
	Health               string    `json:"health"`                // "healthy", "critical"
	LastHeartbeat        time.Time `json:"last_heartbeat"`        // Última resposta válida
	ConsecutiveFailures  int       `json:"consecutive_failures"`  // Contador de falhas
	ConsecutiveSuccesses int       `json:"consecutive_successes"` // Contador de sucessos
}

// PendingRequest representa uma requisição enfileirada aguardando drones disponíveis.
type PendingRequest struct {
	ID              string    `json:"id"`
	SectorID        string    `json:"sector_id"`
	Severity        string    `json:"severity"`
	DurationSeconds int       `json:"duration_seconds"`
	DronesNeeded    int       `json:"drones_needed"`
	QueuedAt        time.Time `json:"queued_at"`
}

// Comando para a máquina de estados.
type Command struct {
	Op              string `json:"op"`
	SectorID        string `json:"sector_id,omitempty"`
	DroneID         int    `json:"drone_id,omitempty"`
	Severity        string `json:"severity,omitempty"`
	DurationSeconds int    `json:"duration_seconds,omitempty"`
	RequestID       string `json:"request_id,omitempty"`
}

// Máquina de estados para gerenciar os drones e fila distribuída.
type FSM struct {
	mu     sync.Mutex
	drones []Drone
	queue  []PendingRequest
	nextID int // Próximo ID sequencial para novos drones
}

var severityDemand = map[string]int{
	"easy":   1,
	"medium": 2,
	"hard":   3,
}

// Implementação da interface FSM do Raft.
func (f *FSM) Apply(raftLog *raft.Log) interface{} {
	f.mu.Lock()
	defer f.mu.Unlock()

	var cmd Command
	if err := json.Unmarshal(raftLog.Data, &cmd); err != nil {
		log.Printf("Failed to unmarshal command: %s", err)
		return nil
	}

	switch cmd.Op {
	case "assign":
		needed := severityDemand[cmd.Severity]
		if needed == 0 {
			needed = 1
		}
		assigned := make([]int, 0, needed)
		finishTime := time.Now().Add(time.Duration(cmd.DurationSeconds) * time.Second)

		for i, drone := range f.drones {
			if drone.Status == "available" && drone.Health != "critical" {
				f.drones[i].AssignedTo = cmd.SectorID
				f.drones[i].Status = "fixing"
				f.drones[i].FinishAt = finishTime
				assigned = append(assigned, drone.ID)
				if len(assigned) == needed {
					break
				}
			}
		}

		if len(assigned) == 0 {
			log.Printf("[ASSIGN] Falha ao alocar %d drone(s) para sector %s (severidade=%s)", needed, cmd.SectorID, cmd.Severity)
			return nil
		}
		log.Printf("[ASSIGN] %d drone(s) alocado(s) para sector %s (severidade=%s). Término em: %s", len(assigned), cmd.SectorID, cmd.Severity, finishTime.Format("15:04:05"))
		return assigned

	case "assign_with_fallback":
		needed := severityDemand[cmd.Severity]
		if needed == 0 {
			needed = 1
		}
		assigned := make([]int, 0, needed)
		finishTime := time.Now().Add(time.Duration(cmd.DurationSeconds) * time.Second)

		for i, drone := range f.drones {
			if drone.Status == "available" && drone.Health != "critical" {
				f.drones[i].AssignedTo = cmd.SectorID
				f.drones[i].Status = "fixing"
				f.drones[i].FinishAt = finishTime
				assigned = append(assigned, drone.ID)
				if len(assigned) == needed {
					log.Printf("[ASSIGN_FALLBACK] Sucesso na primeira tentativa: %d drone(s) para %s", len(assigned), cmd.SectorID)
					return assigned
				}
			}
		}

		if len(assigned) < needed {
			reqID := cmd.RequestID
			if reqID == "" {
				reqID = cmd.SectorID + "-" + time.Now().Format("20060102150405")
			}

			pendingReq := PendingRequest{
				ID:              reqID,
				SectorID:        cmd.SectorID,
				Severity:        cmd.Severity,
				DurationSeconds: cmd.DurationSeconds,
				DronesNeeded:    needed - len(assigned),
				QueuedAt:        time.Now(),
			}
			f.queue = append(f.queue, pendingReq)

			if len(assigned) > 0 {
				log.Printf("[FILA] Alocação parcial para %s: %d de %d drones. Enfileirando %d restante(s) - ID: %s",
					cmd.SectorID, len(assigned), needed, needed-len(assigned), reqID)
				return assigned
			} else {
				log.Printf("[FILA] Sem drones disponíveis para %s. Requisição enfileirada - ID: %s", cmd.SectorID, reqID)
				return map[string]interface{}{"queued": true, "request_id": reqID}
			}
		}
		return assigned

	case "queue_request":
		reqID := cmd.RequestID
		if reqID == "" {
			reqID = cmd.SectorID + "-" + time.Now().Format("20060102150405")
		}

		needed := severityDemand[cmd.Severity]
		if needed == 0 {
			needed = 1
		}

		pendingReq := PendingRequest{
			ID:              reqID,
			SectorID:        cmd.SectorID,
			Severity:        cmd.Severity,
			DurationSeconds: cmd.DurationSeconds,
			DronesNeeded:    needed,
			QueuedAt:        time.Now(),
		}
		f.queue = append(f.queue, pendingReq)
		return map[string]interface{}{"queued": true, "request_id": reqID, "queue_size": len(f.queue)}

	case "process_queue":
		if len(f.queue) == 0 {
			return map[string]interface{}{"processed": 0, "remaining_queue": 0}
		}

		processed := 0
		newQueue := make([]PendingRequest, 0)

		for _, req := range f.queue {
			assigned := make([]int, 0, req.DronesNeeded)
			finishTime := time.Now().Add(time.Duration(req.DurationSeconds) * time.Second)

			for i, drone := range f.drones {
				if drone.Status == "available" && drone.Health != "critical" && len(assigned) < req.DronesNeeded {
					f.drones[i].AssignedTo = req.SectorID
					f.drones[i].Status = "fixing"
					f.drones[i].FinishAt = finishTime
					assigned = append(assigned, drone.ID)
				}
			}

			if len(assigned) == req.DronesNeeded {
				processed++
			} else if len(assigned) > 0 {
				updatedReq := req
				updatedReq.DronesNeeded -= len(assigned)
				newQueue = append(newQueue, updatedReq)
			} else {
				newQueue = append(newQueue, req)
			}
		}

		f.queue = newQueue
		return map[string]interface{}{"processed": processed, "remaining_queue": len(f.queue)}

	case "get_queue":
		queueCopy := make([]PendingRequest, len(f.queue))
		copy(queueCopy, f.queue)
		return queueCopy

	case "finish_fix":
		for i, drone := range f.drones {
			if drone.ID == cmd.DroneID && drone.Status == "fixing" {
				f.drones[i].Status = "available"
				f.drones[i].FinishAt = time.Time{}
				log.Printf("Drone %d finished fixing and remains STATIONED at sector %s", drone.ID, f.drones[i].AssignedTo)
				return true
			}
		}
		return false

	case "get_drones":
		dronesCopy := make([]Drone, len(f.drones))
		copy(dronesCopy, f.drones)
		return dronesCopy

	// Cases das rotinas do 'setor.go'
	case "simulate_drone_failure":
		failed := rand.Intn(100) < 1
		for i, drone := range f.drones {
			if drone.ID == cmd.DroneID {
				if failed {
					f.drones[i].ConsecutiveFailures++
					f.drones[i].ConsecutiveSuccesses = 0
					if f.drones[i].ConsecutiveFailures >= 3 {
						f.drones[i].Health = "critical"
						f.drones[i].Status = "dead"
					}
				}
				return map[string]interface{}{"failed": failed}
			}
		}
		return map[string]interface{}{"failed": false}

	case "heartbeat_drone":
		for i, drone := range f.drones {
			if drone.ID == cmd.DroneID && drone.Health != "critical" {
				f.drones[i].LastHeartbeat = time.Now()
				f.drones[i].ConsecutiveSuccesses++
				f.drones[i].ConsecutiveFailures = 0
				return true
			}
		}
		return false

	case "detect_dead_drones":
		deadDrones := make([]int, 0)
		now := time.Now()
		for i, drone := range f.drones {
			timeSinceHeartbeat := now.Sub(drone.LastHeartbeat).Seconds()
			if drone.Health == "critical" || (drone.Status == "fixing" && timeSinceHeartbeat > 15 && !drone.LastHeartbeat.IsZero()) {
				if drone.Status != "dead" {
					f.drones[i].Status = "dead"
					f.drones[i].Health = "critical"
				}
				deadDrones = append(deadDrones, drone.ID)
			}
		}
		return map[string]interface{}{"dead_drones": deadDrones}

	case "release_sector_drones":
		released := 0
		for i, drone := range f.drones {
			if drone.AssignedTo == cmd.SectorID && drone.Status == "fixing" {
				f.drones[i].Status = "available"
				f.drones[i].FinishAt = time.Time{}
				released++
			}
		}
		if released > 0 {
			log.Printf("[FSM] %d drone(s) liberado(s) do setor caído %s", released, cmd.SectorID)
		}
		return map[string]interface{}{"released": released}

	case "create_drone":
		sector := cmd.SectorID
		if sector == "" {
			log.Printf("[FSM] WARNING: create_drone chamado sem SectorID, usando 'unknown'")
			sector = "unknown"
		}

		newDrone := Drone{
			ID:            f.nextID,
			AssignedTo:    sector,
			Status:        "available",
			Health:        "healthy",
			LastHeartbeat: time.Now(),
		}
		f.nextID++

		// Tenta reutilizar slot de drone morto para evitar crescimento infinito do pool
		reused := false
		for i, drone := range f.drones {
			if drone.Status == "dead" {
				f.drones[i] = newDrone
				reused = true
				log.Printf("[FSM] Drone %d substituiu drone morto no slot %d para %s", newDrone.ID, i, sector)
				break
			}
		}

		if !reused {
			f.drones = append(f.drones, newDrone)
			log.Printf("[FSM] Novo drone %d criado para %s (pool: %d)", newDrone.ID, sector, len(f.drones))
		}
		return newDrone.ID

	default:
		log.Printf("Unknown command: %s", cmd.Op)
		return nil
	}
}

func (f *FSM) Snapshot() (raft.FSMSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return &fsmSnapshot{drones: f.drones, queue: f.queue, nextID: f.nextID}, nil
}

func (f *FSM) Restore(rc io.ReadCloser) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	type FullState struct {
		Drones []Drone          `json:"drones"`
		Queue  []PendingRequest `json:"queue"`
		NextID int              `json:"nextID"`
	}

	var state FullState
	if err := json.NewDecoder(rc).Decode(&state); err != nil {
		rc.Close()
		return json.NewDecoder(rc).Decode(&f.drones)
	}

	f.drones = state.Drones
	f.queue = state.Queue
	if state.NextID > 0 {
		f.nextID = state.NextID
	} else {
		// Compatibilidade com snapshots antigos sem nextID
		f.nextID = len(f.drones)
	}
	return nil
}

type fsmSnapshot struct {
	drones []Drone
	queue  []PendingRequest
	nextID int
}

func (s *fsmSnapshot) Persist(sink raft.SnapshotSink) error {
	state := map[string]interface{}{
		"drones": s.drones,
		"queue":  s.queue,
		"nextID": s.nextID,
	}
	err := json.NewEncoder(sink).Encode(state)
	if err != nil {
		sink.Cancel()
	}
	return err
}

func (s *fsmSnapshot) Release() {}

func NewFSM(initialCount int, sectorID string) *FSM {
	dronesList := make([]Drone, initialCount)
	for i := 0; i < initialCount; i++ {
		dronesList[i] = Drone{
			ID:            i,
			AssignedTo:    sectorID,
			Status:        "available",
			FinishAt:      time.Time{},
			Health:        "healthy",
			LastHeartbeat: time.Now(),
		}
	}
	return &FSM{drones: dronesList, queue: make([]PendingRequest, 0), nextID: initialCount}
}

func (f *FSM) GetAvailableCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	count := 0
	for _, drone := range f.drones {
		if drone.Status == "available" && drone.Health != "critical" {
			count++
		}
	}
	return count
}

func (f *FSM) GetDrones() []Drone {
	f.mu.Lock()
	defer f.mu.Unlock()

	dronesCopy := make([]Drone, len(f.drones))
	copy(dronesCopy, f.drones)
	return dronesCopy
}

func (f *FSM) GetQueue() []PendingRequest {
	f.mu.Lock()
	defer f.mu.Unlock()

	queueCopy := make([]PendingRequest, len(f.queue))
	copy(queueCopy, f.queue)
	return queueCopy
}

func (f *FSM) GetQueueSize() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.queue)
}
