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

// PendingRequest representa uma requisição enfileirada aguardando drones disponíveis.
// Faz parte da Fila Distribuída implementada por Raft.
type PendingRequest struct {
	ID              string    `json:"id"`               // ID único da requisição
	SectorID        string    `json:"sector_id"`        // Setor que solicitou
	Severity        string    `json:"severity"`         // "easy", "medium", "hard"
	DurationSeconds int       `json:"duration_seconds"` // Tempo estimado para conserto
	DronesNeeded    int       `json:"drones_needed"`    // Quantidade de drones necessários
	QueuedAt        time.Time `json:"queued_at"`        // Timestamp do enfileiramento
}

// Comando para a máquina de estados.
type Command struct {
	Op              string `json:"op"`
	SectorID        string `json:"sector_id,omitempty"`
	DroneID         int    `json:"drone_id,omitempty"`
	Severity        string `json:"severity,omitempty"`
	DurationSeconds int    `json:"duration_seconds,omitempty"` // Tempo determinado para o conserto
	RequestID       string `json:"request_id,omitempty"`       // ID da requisição (para queue)
}

// Máquina de estados para gerenciar os drones e fila distribuída.
type FSM struct {
	mu     sync.Mutex
	drones []Drone
	queue  []PendingRequest // Fila distribuída via Raft
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
			log.Printf("[ASSIGN] Falha ao alocar %d drone(s) para sector %s (severidade=%s)", needed, cmd.SectorID, cmd.Severity)
			return nil
		}
		log.Printf("[ASSIGN] %d drone(s) alocado(s) para sector %s (severidade=%s). Término em: %s", len(assigned), cmd.SectorID, cmd.Severity, finishTime.Format("15:04:05"))
		return assigned

	case "assign_with_fallback":
		// Versão melhorada com fallback automático para outros setores
		needed := severityDemand[cmd.Severity]
		if needed == 0 {
			needed = 1
		}
		assigned := make([]int, 0, needed)
		finishTime := time.Now().Add(time.Duration(cmd.DurationSeconds) * time.Second)

		// Primeira tentativa: aloca para o setor solicitado
		for i, drone := range f.drones {
			if drone.Status == "available" {
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

		// Se não conseguiu alocar todos, enfileira a requisição
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
				DronesNeeded:    needed - len(assigned), // Quantidade faltante
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

		if len(assigned) == 0 {
			log.Printf("[ASSIGN_FALLBACK] Falha: sem drones disponíveis no cluster")
			return nil
		}

		return assigned

	case "queue_request":
		// Comando explícito para enfileirar uma requisição
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
		log.Printf("[FILA] Requisição enfileirada - ID: %s para %s (fila tamanho: %d)", reqID, cmd.SectorID, len(f.queue))
		return map[string]interface{}{"queued": true, "request_id": reqID, "queue_size": len(f.queue)}

	case "process_queue":
		// Processa fila: tenta alocar drones para requisições enfileiradas
		if len(f.queue) == 0 {
			return map[string]interface{}{"processed": 0}
		}

		processed := 0
		newQueue := make([]PendingRequest, 0)
		finishTime := time.Now().Add(time.Duration(cmd.DurationSeconds) * time.Second)

		for _, req := range f.queue {
			assigned := make([]int, 0, req.DronesNeeded)

			// Tenta alocar drones para esta requisição
			for i, drone := range f.drones {
				if drone.Status == "available" && len(assigned) < req.DronesNeeded {
					f.drones[i].AssignedTo = req.SectorID
					f.drones[i].Status = "fixing"
					f.drones[i].FinishAt = finishTime
					assigned = append(assigned, drone.ID)
				}
			}

			if len(assigned) == req.DronesNeeded {
				// Requisição completamente satisfeita
				log.Printf("[FILA] Requisição processada e satisfeita - ID: %s (%d drones para %s)",
					req.ID, len(assigned), req.SectorID)
				processed++
			} else if len(assigned) > 0 {
				// Alocação parcial: atualiza requisição e mantém na fila
				updatedReq := req
				updatedReq.DronesNeeded -= len(assigned)
				newQueue = append(newQueue, updatedReq)
				log.Printf("[FILA] Alocação parcial para %s - ID: %s (%d drones, %d pendente)",
					req.SectorID, req.ID, len(assigned), updatedReq.DronesNeeded)
			} else {
				// Nenhum drone disponível, mantém na fila
				newQueue = append(newQueue, req)
			}
		}

		f.queue = newQueue
		log.Printf("[FILA] Processamento concluído: %d requisições satisfeitas, %d permanecendo na fila", processed, len(f.queue))
		return map[string]interface{}{"processed": processed, "remaining_queue": len(f.queue)}

	case "get_queue":
		// Retorna fila atual (cópia para leitura segura)
		queueCopy := make([]PendingRequest, len(f.queue))
		copy(queueCopy, f.queue)
		return queueCopy

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
// Inclui tanto drones quanto fila distribuída.
func (f *FSM) Snapshot() (raft.FSMSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return &fsmSnapshot{drones: f.drones, queue: f.queue}, nil
}

// Implementação da interface Restore do Raft.
// Restaura tanto drones quanto fila distribuída.
func (f *FSM) Restore(rc io.ReadCloser) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	// Tenta decodificar como nova estrutura (com fila)
	type FullState struct {
		Drones []Drone          `json:"drones"`
		Queue  []PendingRequest `json:"queue"`
	}

	var state FullState
	if err := json.NewDecoder(rc).Decode(&state); err != nil {
		// Fallback para estrutura antiga (apenas drones)
		rc.Close()
		return json.NewDecoder(rc).Decode(&f.drones)
	}

	f.drones = state.Drones
	f.queue = state.Queue
	return nil
}

type fsmSnapshot struct {
	drones []Drone
	queue  []PendingRequest
}

// Persiste snapshot incluindo drones e fila.
func (s *fsmSnapshot) Persist(sink raft.SnapshotSink) error {
	state := map[string]interface{}{
		"drones": s.drones,
		"queue":  s.queue,
	}
	err := json.NewEncoder(sink).Encode(state)
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
	return &FSM{drones: drones, queue: make([]PendingRequest, 0)}
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

// GetQueue retorna uma cópia da fila distribuída de forma segura.
// Permite que setores monitorem requisições enfileiradas sem violar mutex.
func (f *FSM) GetQueue() []PendingRequest {
	f.mu.Lock()
	defer f.mu.Unlock()

	queueCopy := make([]PendingRequest, len(f.queue))
	copy(queueCopy, f.queue)
	return queueCopy
}

// GetQueueSize retorna o tamanho atual da fila de forma thread-safe.
func (f *FSM) GetQueueSize() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.queue)
}
