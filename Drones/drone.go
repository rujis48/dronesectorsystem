package drones

import (
	"encoding/json"
	"io"
	"log"
	"sync"
	"time"

	"github.com/hashicorp/raft"
)

// Estrutura do Drone.
type Drone struct {
	ID         int       `json:"id"`
	AssignedTo string    `json:"assigned_to"` // Sempre conterá o ID de um setor (nunca vazio)
	Status     string    `json:"status"`      // "available", "fixing"
	FinishAt   time.Time `json:"finish_at"`   // Momento exato em que o drone deve ser liberado
}

// Comando para a máquina de estados.
type Command struct {
	Op              string `json:"op"`
	SectorID        string `json:"sector_id,omitempty"`
	DroneID         int    `json:"drone_id,omitempty"`
	Severity        string `json:"severity,omitempty"`
	DurationSeconds int    `json:"duration_seconds,omitempty"` // Tempo determinado para o conserto
}

// Máquina de estados para gerenciar os drones.
type FSM struct {
	mu     sync.Mutex
	drones []Drone
}

// Função aplicação da severidade da demanda para os drones.
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

		// Calcula deterministicamente o momento de finalização baseado no tempo enviado no comando
		finishTime := time.Now().Add(time.Duration(cmd.DurationSeconds) * time.Second)

		for i, drone := range f.drones {
			if drone.Status == "available" {
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
			log.Printf("No available drones for sector %s (severity=%s)", cmd.SectorID, cmd.Severity)
			return nil
		}
		log.Printf("Assigned %d drone(s) to sector %s for a %s problem. Finish at: %s", len(assigned), cmd.SectorID, cmd.Severity, finishTime.Format("15:04:05"))
		return assigned

	case "finish_fix":
		for i, drone := range f.drones {
			if drone.ID == cmd.DroneID && drone.Status == "fixing" {
				f.drones[i].Status = "available"
				f.drones[i].FinishAt = time.Time{} // Zera o timer
				// ATENÇÃO: f.drones[i].AssignedTo NÃO é limpo. O drone continua fisicamente no setor atual.
				log.Printf("Drone %d finished fixing and remains STATIONED at sector %s", drone.ID, f.drones[i].AssignedTo)
				return true
			}
		}
		log.Printf("Drone %d not found or not fixing", cmd.DroneID)
		return false

	case "get_drones":
		// Retorna uma cópia para evitar problemas de concorrência na leitura externa via comando Raft
		dronesCopy := make([]Drone, len(f.drones))
		copy(dronesCopy, f.drones)
		return dronesCopy

	default:
		log.Printf("Unknown command: %s", cmd.Op)
		return nil
	}
}

// Implementação da interface Snapshot do Raft.
func (f *FSM) Snapshot() (raft.FSMSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return &fsmSnapshot{drones: f.drones}, nil
}

// Implementação da interface Restore do Raft.
func (f *FSM) Restore(rc io.ReadCloser) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return json.NewDecoder(rc).Decode(&f.drones)
}

type fsmSnapshot struct {
	drones []Drone
}

func (s *fsmSnapshot) Persist(sink raft.SnapshotSink) error {
	err := json.NewEncoder(sink).Encode(s.drones)
	if err != nil {
		sink.Cancel()
	}
	return err
}

func (s *fsmSnapshot) Release() {}

func NewFSM(initialCount int) *FSM {
	drones := make([]Drone, initialCount)
	for i := 0; i < initialCount; i++ {
		// Inicializa todos os drones estacionados no "sector1" para cumprir a regra de negócio
		drones[i] = Drone{ID: i, AssignedTo: "sector1", Status: "available", FinishAt: time.Time{}}
	}
	return &FSM{drones: drones}
}

func (f *FSM) GetAvailableCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	count := 0
	for _, drone := range f.drones {
		if drone.Status == "available" {
			count++
		}
	}
	return count
}

// CORREÇÃO: Método adicionado para expor a leitura local de forma segura
// Permite que a API HTTP dos seguidores leia o estado atual sem violar o consenso
func (f *FSM) GetDrones() []Drone {
	f.mu.Lock()
	defer f.mu.Unlock()

	// Cria e retorna um clone isolado para evitar condições de corrida com gravações em paralelo
	dronesCopy := make([]Drone, len(f.drones))
	copy(dronesCopy, f.drones)
	return dronesCopy
}
