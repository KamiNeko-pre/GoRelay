package connector

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"

	"gorelay/internal/acquisition"
	"gorelay/internal/engine"
)

type checkpointCallbackPayload struct {
	Op       string                 `json:"op"`
	Cursor   any                    `json:"cursor,omitempty"`
	Records  []map[string]any       `json:"records,omitempty"`
	Evidence []acquisition.Response `json:"evidence,omitempty"`
}

type checkpointSession struct {
	token string
	hooks *engine.CheckpointHooks
}

// CheckpointHub hosts scoped internal HTTP callbacks for remote lua-runner
// containers so -child Lua VMs execute checkpoint.get/save without DB credentials.
type CheckpointHub struct {
	mu       sync.RWMutex
	sessions map[string]checkpointSession
}

func NewCheckpointHub() *CheckpointHub {
	return &CheckpointHub{sessions: make(map[string]checkpointSession)}
}

func (h *CheckpointHub) Register(jobID, token string, hooks *engine.CheckpointHooks) func() {
	h.mu.Lock()
	h.sessions[jobID] = checkpointSession{token: token, hooks: hooks}
	h.mu.Unlock()
	return func() {
		h.mu.Lock()
		delete(h.sessions, jobID)
		h.mu.Unlock()
	}
}

func (h *CheckpointHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	jobID := strings.TrimPrefix(r.URL.Path, "/internal/checkpoints/")
	jobID = strings.TrimSpace(jobID)

	h.mu.RLock()
	sess, ok := h.sessions[jobID]
	h.mu.RUnlock()
	if !ok {
		writeCheckpointReply(w, http.StatusNotFound, false, nil, errors.New("checkpoint session expired or unknown"))
		return
	}
	authHeader := r.Header.Get("Authorization")
	if subtle.ConstantTimeCompare([]byte(authHeader), []byte("Bearer "+sess.token)) != 1 {
		writeCheckpointReply(w, http.StatusUnauthorized, false, nil, errors.New("unauthorized checkpoint callback"))
		return
	}

	var payload checkpointCallbackPayload
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4*1024*1024)).Decode(&payload); err != nil {
		writeCheckpointReply(w, http.StatusBadRequest, false, nil, err)
		return
	}
	switch payload.Op {
	case "get":
		cur, err := sess.hooks.Get(r.Context())
		if err != nil {
			writeCheckpointReply(w, http.StatusConflict, false, nil, err)
			return
		}
		writeCheckpointReply(w, http.StatusOK, true, cur, nil)
	case "save":
		err := sess.hooks.Save(r.Context(), payload.Cursor, payload.Records, payload.Evidence)
		if err != nil {
			writeCheckpointReply(w, http.StatusConflict, false, nil, err)
			return
		}
		writeCheckpointReply(w, http.StatusOK, true, nil, nil)
	default:
		writeCheckpointReply(w, http.StatusBadRequest, false, nil, errors.New("unsupported checkpoint op"))
	}
}

func writeCheckpointReply(w http.ResponseWriter, status int, ok bool, cursor any, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	reply := map[string]any{"ok": ok}
	if cursor != nil {
		reply["cursor"] = cursor
	}
	if err != nil {
		reply["error"] = err.Error()
	}
	_ = json.NewEncoder(w).Encode(reply)
}
