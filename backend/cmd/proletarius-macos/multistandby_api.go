package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"
)

func (a *app) registerMultiStandbyRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/multistandby/cards", a.multiCards)
	mux.HandleFunc("GET /api/multistandby/profiles", a.multiProfiles)
	mux.HandleFunc("POST /api/multistandby/start", a.multiOperation)
	mux.HandleFunc("POST /api/multistandby/stop", a.multiOperation)
	mux.HandleFunc("GET /api/multistandby/operations/{id}", func(w http.ResponseWriter, r *http.Request) {
		m := a.multiStandby()
		m.mu.Lock()
		op, ok := m.operations[r.PathValue("id")]
		m.mu.Unlock()
		if !ok {
			writeError(w, 404, "operation not found")
			return
		}
		writeJSON(w, 200, op)
	})
	mux.HandleFunc("POST /api/multistandby/messages/send", a.multiSend)
}
func (a *app) multiCards(w http.ResponseWriter, r *http.Request) {
	m := a.multiStandby()
	err := m.inventory(r.Context())
	profiles := m.snapshot("")
	cards := []map[string]any{}
	seen := map[string]bool{}
	for _, p := range profiles {
		if seen[p.CardID] {
			continue
		}
		seen[p.CardID] = true
		count := 0
		for _, other := range profiles {
			if other.CardID == p.CardID {
				count++
			}
		}
		cards = append(cards, map[string]any{"card_id": p.CardID, "reader": p.Reader, "eid": p.EID, "profile_count": count, "available": err == nil, "detail": multiCardError(err)})
	}
	writeJSON(w, 200, map[string]any{"cards": cards, "boot_id": m.bootID, "detail": multiCardError(err)})
}
func (a *app) multiProfiles(w http.ResponseWriter, r *http.Request) {
	m := a.multiStandby()
	profiles := m.snapshot(r.URL.Query().Get("card_id"))
	var catalog struct {
		Desired []struct {
			ProfileID    string `json:"profile_id"`
			DesiredState string `json:"desired_state"`
		} `json:"desired"`
		Profiles []multiProfile `json:"profiles"`
		Revision uint64         `json:"revision"`
	}
	err := m.store.call(r.Context(), "store/catalog", nil, &catalog)
	if err != nil {
		writeError(w, 503, "多待存储尚未就绪")
		return
	}
	seen := map[string]bool{}
	for _, p := range profiles {
		seen[p.ProfileID] = true
	}
	card := r.URL.Query().Get("card_id")
	for _, p := range catalog.Profiles {
		if !seen[p.ProfileID] && (card == "" || p.CardID == card) {
			p.Health = "offline"
			p.DesiredState = "stopped"
			for _, desired := range catalog.Desired {
				if desired.ProfileID == p.ProfileID {
					p.DesiredState = desired.DesiredState
					break
				}
			}
			p.LastError = "卡片当前不可用；历史短信仍可查看"
			profiles = append(profiles, p)
		}
	}
	writeJSON(w, 200, map[string]any{"profiles": profiles, "boot_id": m.bootID, "revision": catalog.Revision})
}

type multiCommand struct {
	CardID          string   `json:"card_id"`
	ProfileIDs      []string `json:"profile_ids"`
	All             bool     `json:"all"`
	ClientRequestID string   `json:"client_request_id"`
}

func decodeMultiRequest(w http.ResponseWriter, r *http.Request, body any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(body); err != nil {
		writeError(w, 400, "无效请求")
		return false
	}
	return true
}
func (a *app) multiOperation(w http.ResponseWriter, r *http.Request) {
	var body multiCommand
	if !decodeMultiRequest(w, r, &body) {
		return
	}
	if body.CardID == "" || body.ClientRequestID == "" || len(body.ClientRequestID) > 128 || body.All == (len(body.ProfileIDs) > 0) {
		writeError(w, 400, "须指定card_id、client_request_id及profile_ids或all")
		return
	}
	m := a.multiStandby()
	start := strings.HasSuffix(r.URL.Path, "/start")
	sort.Strings(body.ProfileIDs)
	payload, _ := json.Marshal(body)
	fingerprint := r.URL.Path + string(payload)
	m.mu.Lock()
	if old := m.requests[body.ClientRequestID]; old != "" {
		matches := m.requestPayloads[body.ClientRequestID] == fingerprint
		m.mu.Unlock()
		if !matches {
			writeError(w, 409, "请求标识已用于其他操作")
			return
		}
		writeJSON(w, 202, map[string]any{"operation_id": old, "accepted": true})
		return
	}
	profiles := []multiProfile{}
	wanted := map[string]bool{}
	for _, id := range body.ProfileIDs {
		wanted[id] = true
	}
	for id, p := range m.profiles {
		if p.CardID == body.CardID && (body.All || wanted[id]) {
			profiles = append(profiles, p)
			delete(wanted, id)
		}
	}
	if len(profiles) == 0 || len(wanted) != 0 {
		m.mu.Unlock()
		writeError(w, 409, "所选Profile不属于当前卡片或已不可用")
		return
	}
	if m.closed {
		m.mu.Unlock()
		writeError(w, 503, "正在退出")
		return
	}
	if len(m.operations) >= 1000 {
		m.mu.Unlock()
		writeError(w, 429, "操作记录已满，请重新启动应用")
		return
	}
	id := multiToken()
	m.requests[body.ClientRequestID] = id
	m.requestPayloads[body.ClientRequestID] = fingerprint
	m.operations[id] = multiOperation{id, "pending", "等待卡片调度"}
	m.mu.Unlock()
	go func() {
		m.commands.Lock()
		defer m.commands.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		m.mu.Lock()
		m.operations[id] = multiOperation{id, "running", "正在执行"}
		m.mu.Unlock()
		var err error
		if start {
			err = m.start(ctx, profiles)
		} else {
			err = m.stop(ctx, profiles)
		}
		state, detail := "completed", "操作完成"
		if err != nil {
			state = "failed"
			detail = err.Error()
		}
		m.mu.Lock()
		m.operations[id] = multiOperation{id, state, detail}
		m.mu.Unlock()
	}()
	writeJSON(w, 202, map[string]any{"operation_id": id, "accepted": true})
}
func (a *app) multiSend(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ProfileID       string `json:"profile_id"`
		Phone           string `json:"phone"`
		Body            string `json:"body"`
		ClientRequestID string `json:"client_request_id"`
	}
	if !decodeMultiRequest(w, r, &body) {
		return
	}
	if body.ProfileID == "" || strings.TrimSpace(body.Phone) == "" || strings.TrimSpace(body.Body) == "" || body.ClientRequestID == "" {
		writeError(w, 400, "号码、正文及请求标识不能为空")
		return
	}
	m := a.multiStandby()
	m.mu.Lock()
	runtime := m.runtimes[body.ProfileID]
	var session *vowifiSession
	if runtime != nil && runtime.desired {
		session = runtime.session
	}
	m.mu.Unlock()
	if session == nil || !multiSessionReaderCurrent(session) {
		writeError(w, 409, "所选号码当前不可发送")
		return
	}
	session.mu.Lock()
	service := session.imsService
	info := session.simInfo
	ready := service != nil && service.MessagingReady() && info != nil && moduleSMSRouteReady(info.SMSC, info.SMSCTarget)
	session.mu.Unlock()
	if !ready {
		writeError(w, 409, "所选号码的短信通道未就绪")
		return
	}
	var result any
	err := m.store.durable(r.Context(), "queue", map[string]any{"event_id": multiID("queue", body.ProfileID, body.ClientRequestID), "profile_id": body.ProfileID, "client_request_id": body.ClientRequestID, "phone": body.Phone, "body": body.Body}, &result)
	if err != nil {
		writeError(w, 409, err.Error())
		return
	}
	writeJSON(w, 202, result)
}
func (a *app) stopMultiStandbyForShutdown(ctx context.Context) error {
	m := a.multiStandby()
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()
	m.commands.Lock()
	defer m.commands.Unlock()
	profiles := m.snapshot("")
	err := m.stopWithDesired(ctx, profiles, true)
	if errors.Is(err, context.Canceled) {
		return ctx.Err()
	}
	return err
}
