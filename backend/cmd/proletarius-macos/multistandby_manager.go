package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"proletarius.local/core/internal/multistandby"
	"proletarius.local/core/internal/vowifi/imscore"
	"sort"
	"strings"
	"sync"
	"time"
)

type multiOperation struct {
	OperationID string `json:"operation_id"`
	State       string `json:"state"`
	Detail      string `json:"detail"`
}
type multiRuntime struct {
	warning  string
	stop     chan struct{}
	stopOnce sync.Once
	profile  multiProfile
	cancel   context.CancelFunc
	done     chan struct{}
	session  *vowifiSession
	desired  bool
}
type multiStandbyManager struct {
	catalogFingerprint  string
	inventoryAt         time.Time
	inventoryGeneration uint64
	recovered           bool
	app                 *app
	store               *multiStoreClient
	bootID              string
	commands            sync.Mutex
	mu                  sync.Mutex
	profiles            map[string]multiProfile
	runtimes            map[string]*multiRuntime
	operations          map[string]multiOperation
	requests            map[string]string
	requestPayloads     map[string]string
	broker              *multiCardBroker
	ownerToken          string
	original            *multiProfile
	closed              bool
	startupGate         chan struct{}
}

func (a *app) multiStandby() *multiStandbyManager {
	a.multiOnce.Do(func() {
		a.multi = &multiStandbyManager{app: a, store: newMultiStoreClient(), bootID: multiToken(), profiles: map[string]multiProfile{}, runtimes: map[string]*multiRuntime{}, operations: map[string]multiOperation{}, requests: map[string]string{}, requestPayloads: map[string]string{}}
	})
	return a.multi
}
func (m *multiStandbyManager) inventory(ctx context.Context) (resultErr error) {
	m.commands.Lock()
	defer m.commands.Unlock()
	m.mu.Lock()
	active := m.broker != nil
	pendingToken := m.ownerToken
	m.mu.Unlock()
	if active {
		return nil
	}
	if pendingToken != "" {
		if err := m.releaseOwner(pendingToken); err != nil {
			return err
		}
	}
	rm := m.app.readerMgr
	if rm == nil {
		return errors.New("读卡器未初始化")
	}
	rm.mu.Lock()
	inventoryGeneration := rm.generation
	rm.mu.Unlock()
	if inventoryGeneration == m.inventoryGeneration && time.Since(m.inventoryAt) < 30*time.Second {
		return nil
	}
	// Merely opening the new page must not borrow a card from an existing
	// reader network session whose next AKA could otherwise deadlock on IPC.
	vowifiSessionMu.Lock()
	readerBusy := (activeSession != nil && activeSession.sourceKind != "module") || (startingVoWiFi != nil && startingVoWiFi.sourceKind != "module") || (retiringSession != nil && retiringSession.sourceKind != "module")
	vowifiSessionMu.Unlock()
	if readerBusy {
		return errors.New("现有读卡器VoWiFi正在运行，请停止后读取多待卡片清单")
	}
	if !beginReaderVoWiFiOperation() {
		return errors.New("读卡器正在执行其他操作")
	}
	defer endReaderVoWiFiOperation()
	token := multiToken()
	preserveLease := func(err error) {
		m.mu.Lock()
		m.ownerToken = token
		m.mu.Unlock()
		readerVoWiFiMu.Lock()
		readerMultiStandbyActive = true
		readerVoWiFiMu.Unlock()
		if resultErr == nil {
			resultErr = err
		}
	}
	if err := m.store.reservation(ctx, "acquire", token); err != nil {
		var rejection *multiRPCError
		if !errors.As(err, &rejection) || rejection.Status != 409 {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			cleanupErr := m.store.reservation(cleanupCtx, "release", token)
			cancel()
			if cleanupErr != nil {
				preserveLease(cleanupErr)
			}
		}
		return err
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := m.store.reservation(cleanupCtx, "release", token); err != nil {
			preserveLease(err)
		}
	}()
	release, err := rm.beginCardOperationContext(ctx)
	if err != nil {
		return err
	}
	var releaseOnce sync.Once
	unlockCard := func() { releaseOnce.Do(release) }
	defer unlockCard()
	manager, ok := rm.ESIMManager()
	if !ok {
		return errors.New("请先连接含eSIM的读卡器")
	}
	rm.mu.Lock()
	reader := rm.readerName
	rm.mu.Unlock()
	groups, err := manager.GetProfiles()
	if err != nil {
		return err
	}
	profiles := []multiProfile{}
	for _, group := range groups {
		if strings.TrimSpace(group.EID) == "" {
			continue
		}
		cardID := multiID("card", reader, group.EID)
		for _, item := range group.Profiles {
			if item.ICCID == "" {
				continue
			}
			name := item.Name
			if name == "" {
				name = item.ServiceProviderName
			}
			p := multiProfile{ProfileID: multiID("profile", group.EID, item.ICCID), CardID: cardID, EID: group.EID, ICCID: item.ICCID, DisplayName: name, PhysicalEnabled: item.State == 1, DesiredState: "stopped", AuthState: "idle", TunnelState: "disconnected", IMSState: "idle", Health: "stopped", AID: group.AIDHex, Reader: reader}
			profiles = append(profiles, p)
		}
	}
	unlockCard()
	catalog := []map[string]any{}
	for _, p := range profiles {
		catalog = append(catalog, map[string]any{"profile_id": p.ProfileID, "card_id": p.CardID, "eid": p.EID, "iccid": p.ICCID, "display_name": p.DisplayName, "phone_number": p.PhoneNumber, "tombstone": false})
	}
	sort.Slice(catalog, func(i, j int) bool { return catalog[i]["profile_id"].(string) < catalog[j]["profile_id"].(string) })
	catalogBytes, _ := json.Marshal(catalog)
	fingerprint := multiID("catalog", string(catalogBytes))
	if fingerprint != m.catalogFingerprint {
		if err := m.store.durable(ctx, "catalog", map[string]any{"event_id": multiToken(), "profiles": catalog}, nil); err != nil {
			return err
		}
		m.catalogFingerprint = fingerprint
	}
	m.inventoryAt = time.Now()
	m.inventoryGeneration = inventoryGeneration
	m.mu.Lock()
	m.profiles = map[string]multiProfile{}
	for _, p := range profiles {
		m.profiles[p.ProfileID] = p
	}
	m.mu.Unlock()
	if !m.recovered {
		var state struct {
			Desired []struct {
				ProfileID    string `json:"profile_id"`
				DesiredState string `json:"desired_state"`
			} `json:"desired"`
		}
		if err := m.store.call(ctx, "store/catalog", nil, &state); err != nil {
			return err
		}
		m.recovered = true
		restore := []multiProfile{}
		for _, desired := range state.Desired {
			if desired.DesiredState == "keep_online" {
				for _, p := range profiles {
					if p.ProfileID == desired.ProfileID {
						restore = append(restore, p)
					}
				}
			}
		}
		if len(restore) > 0 {
			sort.Slice(restore, func(i, j int) bool { return restore[i].ProfileID < restore[j].ProfileID })
			chosen := restore[0].CardID
			oneCard := restore[:0]
			for _, p := range restore {
				if p.CardID == chosen {
					oneCard = append(oneCard, p)
				}
			}
			restore = oneCard
			go func() {
				m.commands.Lock()
				defer m.commands.Unlock()
				m.mu.Lock()
				closed := m.closed
				m.mu.Unlock()
				if closed {
					return
				}
				ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
				defer cancel()
				_ = m.start(ctx, restore)
			}()
		}
	}
	return nil
}
func (m *multiStandbyManager) snapshot(card string) []multiProfile {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []multiProfile{}
	for id, p := range m.profiles {
		if card != "" && p.CardID != card {
			continue
		}
		if r := m.runtimes[id]; r != nil {
			p = r.profile
			if r.session != nil {
				r.session.mu.Lock()
				p.TunnelState = string(r.session.tunnelState)
				p.IMSState = string(r.session.imsState)
				service := r.session.imsService
				p.LastError = r.session.lastError
				if p.LastError == "" {
					p.LastError = r.warning
				}
				p.PhoneNumber = r.session.simInfo.MSISDN
				if r.session.imsResult != nil && !isMissingPhoneNumber(r.session.imsResult.MSISDN) {
					p.PhoneNumber = r.session.imsResult.MSISDN
				}
				r.session.mu.Unlock()
				p.SMSReady = r.desired && service != nil && service.MessagingReady() && moduleSMSRouteReady(r.session.simInfo.SMSC, r.session.simInfo.SMSCTarget)
				if p.SMSReady {
					p.Health = "ready"
				}
			}
		}
		if m.broker != nil && p.EID == m.broker.eid {
			p.PhysicalEnabled = m.broker.currentProfileID() == p.ProfileID
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ProfileID < out[j].ProfileID })
	return out
}
func (m *multiStandbyManager) acquire(ctx context.Context, p multiProfile) error {
	if m.broker != nil {
		if m.broker.eid != p.EID || m.broker.reader != p.Reader {
			return errors.New("请先停止当前卡片的所有Profile")
		}
		return nil
	}
	m.mu.Lock()
	token := m.ownerToken
	m.mu.Unlock()
	readerVoWiFiMu.Lock()
	if readerVoWiFiOperationActive || readerNativeSessionLeaseActive || (readerMultiStandbyActive && token == "") {
		readerVoWiFiMu.Unlock()
		return errors.New("读卡器繁忙")
	}
	vowifiSessionMu.Lock()
	readerBusy := (activeSession != nil && activeSession.sourceKind != "module") || (startingVoWiFi != nil && startingVoWiFi.sourceKind != "module") || (retiringSession != nil && retiringSession.sourceKind != "module")
	vowifiSessionMu.Unlock()
	if readerBusy {
		readerVoWiFiMu.Unlock()
		return errors.New("请先停止现有读卡器VoWiFi会话")
	}
	readerMultiStandbyActive = true
	readerVoWiFiMu.Unlock()
	if token == "" {
		token = multiToken()
	}
	m.mu.Lock()
	m.ownerToken = token
	m.mu.Unlock()
	if err := m.store.reservation(ctx, "acquire", token); err != nil {
		var rejected *multiRPCError
		if errors.As(err, &rejected) && rejected.Status == 409 {
			m.mu.Lock()
			m.ownerToken = ""
			m.mu.Unlock()
			readerVoWiFiMu.Lock()
			readerMultiStandbyActive = false
			readerVoWiFiMu.Unlock()
		} else {
			_ = m.releaseOwner(token)
		}
		return err
	}
	rm := m.app.readerMgr
	rm.mu.Lock()
	generation, reader := rm.generation, rm.readerName
	rm.mu.Unlock()
	if reader != p.Reader {
		_ = m.releaseOwner(token)
		return errors.New("读卡器选择已变化")
	}
	broker := &multiCardBroker{rm: rm, queue: multistandby.NewScheduler(), reader: reader, generation: generation, eid: p.EID}
	info, err := broker.currentIdentity(ctx)
	if err != nil {
		broker.queue.Close()
		_ = m.releaseOwner(token)
		return err
	}
	var original *multiProfile
	m.mu.Lock()
	for _, candidate := range m.profiles {
		if candidate.CardID == p.CardID && normalizeSIMIdentifier(candidate.ICCID) == normalizeSIMIdentifier(info.ICCID) {
			copy := candidate
			original = &copy
			break
		}
	}
	m.mu.Unlock()
	if original == nil {
		broker.queue.Close()
		_ = m.releaseOwner(token)
		return errors.New("当前物理Profile不在所选卡片清单中，请刷新后重试")
	}
	broker.markProfile(*original)
	m.mu.Lock()
	m.broker = broker
	m.original = original
	m.mu.Unlock()
	return nil
}
func (m *multiStandbyManager) start(ctx context.Context, profiles []multiProfile) error {
	m.mu.Lock()
	closed := m.closed
	m.mu.Unlock()
	if closed {
		return errors.New("正在退出，已取消多待启动")
	}
	if len(profiles) == 0 {
		return errors.New("未选择Profile")
	}
	if err := m.acquire(ctx, profiles[0]); err != nil {
		return err
	}
	for _, p := range profiles {
		m.mu.Lock()
		r := m.runtimes[p.ProfileID]
		running := r != nil && r.desired
		m.mu.Unlock()
		if running {
			continue
		}
		if err := m.store.durable(ctx, "desired", map[string]any{"event_id": multiToken(), "profile_id": p.ProfileID, "desired_state": "keep_online"}, nil); err != nil {
			m.mu.Lock()
			empty := len(m.runtimes) == 0
			m.mu.Unlock()
			if empty {
				_ = m.stopWithDesired(context.Background(), nil, true)
			}
			return err
		}
		runCtx, cancel := context.WithCancel(context.Background())
		p.DesiredState = "keep_online"
		p.AuthState = "queued"
		p.Health = "starting"
		r = &multiRuntime{profile: p, cancel: cancel, done: make(chan struct{}), stop: make(chan struct{}), desired: true}
		m.mu.Lock()
		m.runtimes[p.ProfileID] = r
		broker := m.broker
		m.mu.Unlock()
		go m.run(runCtx, r, broker)
	}
	return nil
}
func (m *multiStandbyManager) run(ctx context.Context, r *multiRuntime, broker *multiCardBroker) {
	defer close(r.done)
	m.mu.Lock()
	frozen := r.profile
	m.mu.Unlock()
	for attempt := 0; ctx.Err() == nil; attempt++ {
		releaseStartup, err := m.acquireStartup(ctx, r.stop)
		if err != nil {
			return
		}
		var info *usimInfo
		err = broker.withProfile(ctx, frozen, func(_ context.Context, current *usimInfo) error { copy := *current; info = &copy; return nil })
		var s *vowifiSession
		failed := make(chan error, 1)
		generationCtx, generationCancel := context.WithCancel(ctx)
		if err == nil {
			s, err = prepareReaderVoWiFiSession(generationCtx, m.app, "", "", 0, &vowifiStartup{multiInfo: info, multiReader: broker.reader, multiGeneration: broker.generation}, 0)
		}
		if err == nil {
			adapter := &multiAKA{broker: broker, profile: frozen, ctx: generationCtx, info: info, imei: s.tunnelCfg.IMEI}
			s.multiAuth = adapter
			s.cancel = generationCancel
			s.multiFailure = func(e error) {
				select {
				case failed <- e:
				default:
				}
			}
			s.multiIncoming = func(event imscore.IncomingSMSEvent) error {
				persistCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()
				return m.inbound(persistCtx, frozen, event)
			}
			m.mu.Lock()
			r.session = s
			r.profile.SessionGeneration++
			r.profile.AuthState = "authenticating"
			r.profile.PhoneNumber = info.PhoneNumber
			m.mu.Unlock()
			err = s.run(generationCtx, m.app)
			close(s.runDone)
			if err == nil {
				// run returns only after the first IMS registration completes.
				// Keep this session online while the next profile starts.
				releaseStartup()
				m.mu.Lock()
				r.profile.AuthState = "ready"
				m.mu.Unlock()
				ticker := time.NewTicker(time.Second)
				running := true
				for running {
					select {
					case <-r.stop:
						err = context.Canceled
						running = false
					case <-ctx.Done():
						err = ctx.Err()
						running = false
					case err = <-failed:
						running = false
					case <-ticker.C:
						m.flush(generationCtx, r, s)
					}
				}
				ticker.Stop()
			}
		}
		// A generation is completely retired before a new SWu/netstack/IMS exists.
		if s != nil {
			m.closeSession(s)
			if s.swuSession != nil {
				<-s.swuDone
			}
		}
		generationCancel()
		// On startup failure, retain the gate until the old AKA/transport
		// generation has actually exited; another prepare must not race it.
		releaseStartup()
		m.mu.Lock()
		if r.session == s {
			r.session = nil
		}
		r.profile.SMSReady = false
		r.profile.AuthState = "backoff"
		r.profile.Health = "degraded"
		r.profile.LastError = multiCardError(err)
		m.mu.Unlock()
		select {
		case <-r.stop:
			return
		default:
		}
		if ctx.Err() != nil {
			return
		}
		delay := time.Duration(2+attempt*2) * time.Second
		if delay > 60*time.Second {
			delay = 60 * time.Second
		}
		select {
		case <-r.stop:
			return
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
	}
}

// One manager owns one physical card reservation. Serialize complete startup,
// not just APDUs: a new profile's preparation must not consume the previous
// profile's EAP challenge window. Established sessions do not hold this gate.
func (m *multiStandbyManager) acquireStartup(ctx context.Context, stop <-chan struct{}) (func(), error) {
	m.mu.Lock()
	if m.startupGate == nil {
		m.startupGate = make(chan struct{}, 1)
	}
	gate := m.startupGate
	m.mu.Unlock()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-stop:
		return nil, context.Canceled
	default:
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-stop:
		return nil, context.Canceled
	case gate <- struct{}{}:
	}
	release := sync.OnceFunc(func() { <-gate })
	// Cancellation may have raced acquisition of an available permit.
	select {
	case <-ctx.Done():
		release()
		return nil, ctx.Err()
	case <-stop:
		release()
		return nil, context.Canceled
	default:
		return release, nil
	}
}

func (m *multiStandbyManager) closeSession(s *vowifiSession) {
	// IMS deregistration runs while the tunnel is still alive. Drain SMS first.
	s.imsOpMu.Lock()
	s.mu.Lock()
	service := s.imsService
	s.imsService = nil
	s.mu.Unlock()
	if service != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = service.Close(ctx)
		cancel()
	}
	s.imsOpMu.Unlock()
	s.cleanupResources()
}
func (r *multiRuntime) requestStop() {
	r.stopOnce.Do(func() {
		close(r.stop)
		go func() {
			select {
			case <-r.done:
			case <-time.After(5 * time.Second):
				r.cancel()
			}
		}()
	})
}
func (m *multiStandbyManager) stop(ctx context.Context, profiles []multiProfile) error {
	return m.stopWithDesired(ctx, profiles, false)
}
func (m *multiStandbyManager) stopWithDesired(ctx context.Context, profiles []multiProfile, preserveDesired bool) error {
	var first error
	for _, p := range profiles {
		if !preserveDesired {
			if err := m.store.durable(ctx, "desired", map[string]any{"event_id": multiToken(), "profile_id": p.ProfileID, "desired_state": "stopped"}, nil); err != nil && first == nil {
				first = err
			}
		}
		m.mu.Lock()
		r := m.runtimes[p.ProfileID]
		if r != nil {
			r.desired = false
			r.requestStop()
		}
		m.mu.Unlock()
	}
	for _, p := range profiles {
		m.mu.Lock()
		r := m.runtimes[p.ProfileID]
		m.mu.Unlock()
		if r != nil {
			<-r.done
			m.mu.Lock()
			delete(m.runtimes, p.ProfileID)
			m.mu.Unlock()
		}
	}
	m.mu.Lock()
	empty := len(m.runtimes) == 0
	broker := m.broker
	original := m.original
	token := m.ownerToken
	m.mu.Unlock()
	if !empty {
		return first
	}
	if broker != nil {
		if original != nil {
			restoreCtx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			err := broker.withProfile(restoreCtx, *original, func(context.Context, *usimInfo) error { return nil })
			cancel()
			if err != nil && first == nil {
				first = fmt.Errorf("会话已停止，原Profile恢复未确认: %w", err)
			}
		}
		broker.queue.Close()
		m.mu.Lock()
		m.broker = nil
		m.original = nil
		m.mu.Unlock()
	}
	if token != "" {
		if err := m.releaseOwner(token); err != nil {
			return fmt.Errorf("会话已停止，卡片占用释放待确认: %w", err)
		}
	}
	m.mu.Lock()
	m.broker = nil
	m.original = nil
	m.mu.Unlock()
	return first
}
func (m *multiStandbyManager) releaseOwner(token string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := m.store.reservation(ctx, "release", token); err != nil {
		return err
	}
	m.mu.Lock()
	m.ownerToken = ""
	m.mu.Unlock()
	readerVoWiFiMu.Lock()
	readerMultiStandbyActive = false
	readerVoWiFiMu.Unlock()
	return nil
}
func (m *multiStandbyManager) flush(ctx context.Context, r *multiRuntime, s *vowifiSession) {
	s.mu.Lock()
	service := s.imsService
	ready := service != nil && service.MessagingReady()
	s.mu.Unlock()
	if !ready || !multiSessionReaderCurrent(s) {
		return
	}
	var pending struct {
		Messages []multiStoredMessage `json:"messages"`
	}
	if m.store.call(ctx, "store/pending?profile_id="+r.profile.ProfileID, nil, &pending) != nil {
		return
	}
	for _, message := range pending.Messages {
		if ctx.Err() != nil {
			return
		}
		m.mu.Lock()
		desired := r.desired
		m.mu.Unlock()
		if !desired {
			return
		}
		attempt := multiToken()
		if m.store.durable(ctx, "claim", map[string]any{"event_id": multiToken(), "profile_id": r.profile.ProfileID, "message_id": message.MessageID, "attempt_token": attempt}, nil) != nil {
			continue
		}
		_, err := m.app.sendBoundVoWiFiSMS(s, "multistandby", message.Peer, message.Body)
		state := "accepted"
		detail := ""
		if err != nil {
			state = "unknown"
			detail = "发送结果未确认，请核对后再决定是否重新发送"
		}
		outcomeCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		persistErr := m.store.durable(outcomeCtx, "outcome", map[string]any{"event_id": multiToken(), "profile_id": r.profile.ProfileID, "message_id": message.MessageID, "attempt_token": attempt, "state": state, "error": detail}, nil)
		cancel()
		if persistErr != nil {
			return
		}
	}
}
func (m *multiStandbyManager) inbound(ctx context.Context, p multiProfile, event imscore.IncomingSMSEvent) error {
	// Network retries deduplicate independently of local session generation.
	dedup := multiID("mt", p.ProfileID, hex.EncodeToString(event.RPDU))
	messageID := dedup
	kind := "message"
	if event.StatusReport {
		kind = "status_report"
	} else if !event.Decoded {
		kind = "undecoded"
	}
	body := map[string]any{"event_id": multiToken(), "profile_id": p.ProfileID, "message_id": messageID, "sender": event.Sender, "body": event.Content, "received_at_ms": event.Timestamp.UnixMilli(), "raw_rpdu_hex": hex.EncodeToString(event.RPDU), "raw_tpdu_hex": hex.EncodeToString(event.TPDU), "dedup_key": dedup, "kind": kind}
	if event.Timestamp.IsZero() {
		body["received_at_ms"] = time.Now().UnixMilli()
	}
	if event.Concat != nil {
		part := event.Concat
		body["message_id"] = dedup
		body["part"] = map[string]any{"reference": fmt.Sprint(part.RefBits, ":", part.Reference), "index": part.Sequence, "total": part.Total, "auto_group": true}
	}
	if event.StatusReport {
		body["report"] = map[string]any{"status": event.DeliveryStatus, "status_code": event.StatusCode, "message_reference": event.MessageReference}
	}
	var stored struct {
		State string `json:"state"`
	}
	err := m.store.durable(ctx, "inbound", body, &stored)
	if err != nil && event.Concat != nil { // Preserve a conflicting/ambiguous group without falsely combining messages.
		body["event_id"] = multiToken()
		body["message_id"] = dedup
		body["dedup_key"] = dedup + "_raw"
		body["kind"] = "undecoded"
		delete(body, "part")
		err = m.store.durable(ctx, "inbound", body, &stored)
	}
	if err == nil && stored.State == "undecoded" {
		m.mu.Lock()
		if runtime := m.runtimes[p.ProfileID]; runtime != nil {
			runtime.warning = "收到未能完整解析或分组有歧义的短信，原始记录已保存，未与其他短信合并"
		}
		m.mu.Unlock()
	}
	return err
}

// Invalidating a physical epoch immediately revokes outbound admission. Actual
// workers and IMS inbox callbacks are drained before releasing the card owner.
func (m *multiStandbyManager) invalidateReaderGeneration(generation uint64) {
	m.mu.Lock()
	if m.broker == nil || m.broker.generation != generation {
		m.mu.Unlock()
		return
	}
	for _, r := range m.runtimes {
		r.desired = false
		r.profile.Health = "offline"
		r.profile.LastError = "读卡器已断开"
		r.requestStop()
	}
	m.mu.Unlock()
	go func() {
		m.commands.Lock()
		defer m.commands.Unlock()
		profiles := m.snapshot("")
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		_ = m.stopWithDesired(ctx, profiles, true)
		m.recovered = false
	}()
}
func multiSessionReaderCurrent(s *vowifiSession) bool {
	if s == nil || s.multiAuth == nil {
		return false
	}
	broker := s.multiAuth.broker
	broker.rm.mu.Lock()
	defer broker.rm.mu.Unlock()
	return broker.rm.readerName == broker.reader && broker.rm.generation == broker.generation
}

// Recovery is driven by durable intent, never by visiting the new page. An
// ordinary launch with no keep_online intent performs no card discovery.
func (a *app) startMultiStandbyRecovery(ctx context.Context) {
	m := a.multiStandby()
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			if a.readerMgr == nil || !a.readerMgr.hasSelectedReader() {
				continue
			}
			m.commands.Lock()
			m.mu.Lock()
			active, closed := m.broker != nil, m.closed
			m.mu.Unlock()
			recovered := m.recovered
			m.commands.Unlock()
			if active || closed || recovered {
				continue
			}
			var state struct {
				Desired []struct {
					DesiredState string `json:"desired_state"`
				} `json:"desired"`
			}
			queryCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
			err := m.store.call(queryCtx, "store/catalog", nil, &state)
			cancel()
			if err != nil {
				continue
			}
			wanted := false
			for _, d := range state.Desired {
				if d.DesiredState == "keep_online" {
					wanted = true
					break
				}
			}
			if !wanted {
				continue
			}
			_ = m.inventory(ctx)
		}
	}()
}
