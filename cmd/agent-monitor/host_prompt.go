package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

// hostPrompt is a first-run question the agent shows before it starts a session.
type hostPrompt struct {
	Kind    string             `json:"kind"`
	Agent   string             `json:"agent"`
	Folder  string             `json:"folder,omitempty"`
	Title   string             `json:"title,omitempty"`
	Text    string             `json:"text"`
	Options []hostPromptOption `json:"options"`

	cursor int   // option index under the cursor, -1 when the screen shows none
	digits []int // option numbers as drawn, 0 when unnumbered
}

type hostPromptOption struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

const (
	hostScreenTailLines = 8
	// The trust header must sit in the last screenful, and the options start soon after it.
	hostPromptHeaderWindow = 24
	hostPromptOptionsGap   = 12
	// A host past these has done real work, so a dialog on its screen is not the first-run question.
	hostPromptMaxOutBytes = 64 << 10
	hostPromptMaxLines    = 40
)

var (
	hostViewDial  = 500 * time.Millisecond
	hostViewTotal = 2 * time.Second
	hostViewTTL   = time.Second
	hostViewMu    sync.Mutex
	hostViews     = map[string]hostViewEntry{}
)

var (
	trustHeaderMarkers = []struct{ agent, text string }{
		{"claude", "is this a project you created or one you trust"},
		{"claude", "do you trust the files in this folder"},
		{"codex", "trust this folder?"},
		{"codex", "do you trust the contents of this directory"},
		{"codex", "allow codex to work in this folder"},
	}
	trustGenericHeader = regexp.MustCompile(`(?i)\btrust\b.*\b(folder|directory|project)\b|\b(folder|directory|project)\b.*\btrust`)
	promptCursorRunes  = "❯›>▶➜→"
	promptNumbered     = regexp.MustCompile(`^(\d{1,2})[.)]\s+`)
	promptFooter       = regexp.MustCompile(`(?i)enter to (confirm|continue|select)|esc to (cancel|exit)|press enter`)
	trustDenyWords     = regexp.MustCompile(`(?i)\b(exit|quit|cancel)\b|\bdo(n't|n’t| not) trust\b`)
	hostAnswerSettle   = 200 * time.Millisecond
	hostAnswerQuiet    = 4 * time.Second
	hostAnswerMu       sync.Mutex
	hostAnsweredAt     = map[string]time.Time{}
	hostAnswering      = map[string]struct{}{}
)

// promptLine is one screen row with borders blanked, its label and the label's column.
type promptLine struct {
	cursor bool
	num    int
	col    int
	label  string
}

func parsePromptLine(s string) (promptLine, bool) {
	rs := []rune(s)
	for i, r := range rs {
		if strings.ContainsRune("│┃║", r) {
			rs[i] = ' '
		}
	}
	i := 0
	for i < len(rs) && unicode.IsSpace(rs[i]) {
		i++
	}
	if i == len(rs) {
		return promptLine{}, false
	}
	var pl promptLine
	if strings.ContainsRune(promptCursorRunes, rs[i]) && i+1 < len(rs) && unicode.IsSpace(rs[i+1]) {
		pl.cursor = true
		i++
		for i < len(rs) && unicode.IsSpace(rs[i]) {
			i++
		}
	}
	rest := string(rs[i:])
	if m := promptNumbered.FindStringSubmatch(rest); m != nil {
		for _, c := range m[1] {
			pl.num = pl.num*10 + int(c-'0')
		}
		i += len([]rune(m[0]))
	}
	pl.col = i
	pl.label = strings.TrimSpace(string(rs[i:]))
	return pl, pl.label != ""
}

func classifyTrustOption(label string) string {
	l := strings.ToLower(label)
	switch {
	case trustDenyWords.MatchString(l):
		return "deny"
	case strings.Contains(l, "trust") || strings.HasPrefix(l, "yes") || strings.HasPrefix(l, "allow"):
		return "trust"
	}
	return ""
}

// detectTrustPrompt finds a folder trust dialog on a rendered screen; nil when none is visible.
func detectTrustPrompt(screen, agent string) *hostPrompt {
	lines := strings.Split(strings.TrimRight(screen, "\n "), "\n")
	from := max(0, len(lines)-hostPromptHeaderWindow)
	head, who := -1, ""
	for i := from; i < len(lines); i++ {
		low := strings.ToLower(lines[i])
		for _, m := range trustHeaderMarkers {
			if strings.Contains(low, m.text) {
				head, who = i, m.agent
				break
			}
		}
		if head >= 0 {
			break
		}
	}
	if head < 0 {
		for i := from; i < len(lines); i++ {
			if trustGenericHeader.MatchString(lines[i]) {
				head = i
				break
			}
		}
	}
	if head < 0 {
		return nil
	}
	if agent != "" {
		who = agent
	}
	// The option list is the cursor row plus its neighbours whose label starts in the same column.
	first, last, cur := -1, -1, -1
	for i := head; i < len(lines) && i <= head+hostPromptOptionsGap+9; i++ {
		if pl, ok := parsePromptLine(lines[i]); ok && pl.cursor {
			cur = i
			break
		}
	}
	at := func(i int) (promptLine, bool) { return parsePromptLine(lines[i]) }
	if cur >= 0 {
		pc, _ := at(cur)
		first, last = cur, cur
		for i := cur - 1; i > head; i-- {
			pl, ok := at(i)
			if !ok || pl.col != pc.col || pl.cursor {
				break
			}
			first = i
		}
		for i := cur + 1; i < len(lines); i++ {
			pl, ok := at(i)
			if !ok || pl.col != pc.col || pl.cursor || promptFooter.MatchString(pl.label) {
				break
			}
			last = i
		}
	} else {
		for i := head + 1; i < len(lines) && (first >= 0 || i <= head+hostPromptOptionsGap); i++ {
			pl, ok := at(i)
			if ok && pl.num > 0 {
				if first < 0 {
					first = i
				}
				last = i
			} else if first >= 0 {
				break
			}
		}
	}
	if first < 0 || last-first < 1 || first-head > hostPromptOptionsGap {
		return nil
	}
	p := &hostPrompt{Kind: "trust", Agent: who, cursor: -1}
	seen := map[string]bool{}
	for i := first; i <= last; i++ {
		pl, _ := at(i)
		id := classifyTrustOption(pl.label)
		if id == "" || seen[id] {
			id = "option" + strconv.Itoa(i-first+1)
		}
		seen[id] = true
		if pl.cursor {
			p.cursor = i - first
		}
		p.Options = append(p.Options, hostPromptOption{ID: id, Label: pl.label})
		p.digits = append(p.digits, pl.num)
	}
	if !seen["trust"] {
		return nil
	}
	var text []string
	for i := head; i < first; i++ {
		if t := strings.TrimSpace(strings.Trim(lines[i], "│┃║ ")); t != "" {
			text = append(text, t)
		}
	}
	p.Text = strings.Join(text, " ")
	return p
}

// detectStartPrompt finds any first-run menu: the folder trust dialog first, then any other menu.
func detectStartPrompt(screen, agent string) *hostPrompt {
	if p := detectTrustPrompt(screen, agent); p != nil {
		return p
	}
	return detectMenuPrompt(screen, agent)
}

var menuDenyLabel = regexp.MustCompile(`(?i)^(no\b|continue without|skip\b|don.t\b|do not\b)`)

// detectMenuPrompt finds a menu with a cursor row and an "Enter to confirm" or "Esc to cancel" footer,
// such as Claude's "New MCP server found" dialog. Its title is the first line above the options.
func detectMenuPrompt(screen, agent string) *hostPrompt {
	lines := strings.Split(strings.TrimRight(screen, "\n "), "\n")
	from := max(0, len(lines)-hostPromptHeaderWindow)
	foot := -1
	for i := len(lines) - 1; i >= from; i-- {
		if promptFooter.MatchString(lines[i]) {
			foot = i
			break
		}
	}
	if foot < 0 {
		return nil
	}
	cur := -1
	for i := foot - 1; i >= from && i >= foot-hostPromptOptionsGap; i-- {
		if pl, ok := parsePromptLine(lines[i]); ok && pl.cursor {
			cur = i
			break
		}
	}
	if cur < 0 {
		return nil
	}
	pc, _ := parsePromptLine(lines[cur])
	first, last := cur, cur
	for i := cur - 1; i >= from; i-- {
		pl, ok := parsePromptLine(lines[i])
		if !ok || pl.col != pc.col || pl.cursor {
			break
		}
		first = i
	}
	for i := cur + 1; i < foot; i++ {
		pl, ok := parsePromptLine(lines[i])
		if !ok || pl.col != pc.col || pl.cursor || promptFooter.MatchString(pl.label) {
			break
		}
		last = i
	}
	if last-first < 1 {
		return nil
	}
	top := first - 1
	for top >= from && top >= first-hostPromptOptionsGap && !isBoxRule(strings.TrimSpace(lines[top])) {
		top--
	}
	var text []string
	for i := top + 1; i < first; i++ {
		if t := strings.TrimSpace(strings.Trim(lines[i], "│┃║ ")); t != "" {
			text = append(text, t)
		}
	}
	if len(text) == 0 {
		return nil
	}
	p := &hostPrompt{Kind: "menu", Agent: agent, Title: text[0], Text: strings.Join(text[1:], " "), cursor: -1}
	if strings.Contains(strings.ToLower(text[0]), "mcp server") {
		p.Kind = "mcp"
	}
	seen := map[string]bool{}
	for i := first; i <= last; i++ {
		pl, _ := parsePromptLine(lines[i])
		id := "option" + strconv.Itoa(i-first+1)
		if menuDenyLabel.MatchString(pl.label) && !seen["deny"] {
			id = "deny"
		}
		seen[id] = true
		if pl.cursor {
			p.cursor = i - first
		}
		p.Options = append(p.Options, hostPromptOption{ID: id, Label: pl.label})
		p.digits = append(p.digits, pl.num)
	}
	return p
}

func (p *hostPrompt) index(id string) int {
	for i, o := range p.Options {
		if o.ID == id {
			return i
		}
	}
	return -1
}

// screenTail is the last lines of a screen with trailing blank rows dropped.
func screenTail(screen string, n int) []string {
	lines := strings.Split(strings.TrimRight(screen, "\n "), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	for len(lines) > 0 && strings.TrimSpace(lines[0]) == "" {
		lines = lines[1:]
	}
	return lines
}

func hostAgentName(h *HostInfo) string {
	if h.Tool != "" {
		return canonicalAgentName(string(h.Tool))
	}
	return canonicalAgentName(h.Agent)
}

func canonicalAgentName(s string) string {
	s = strings.ToLower(s)
	switch {
	case strings.Contains(s, "claude"):
		return "claude"
	case strings.Contains(s, "codex"):
		return "codex"
	}
	return ""
}

func recentlyAnswered(id string) bool {
	hostAnswerMu.Lock()
	defer hostAnswerMu.Unlock()
	t, ok := hostAnsweredAt[id]
	if ok && time.Since(t) >= hostAnswerQuiet {
		delete(hostAnsweredAt, id)
		return false
	}
	return ok
}

// hostView is one read of an unlinked host: its visible screen and, when a dialog shows, its output size.
type hostView struct {
	screen string // visible rows
	lines  int    // non-blank rows in the host's recent history
	out    int64  // bytes the agent printed since start, -1 when unknown
}

type hostViewEntry struct {
	at  time.Time
	v   hostView
	err error
}

// fresh reports whether the host is still at its first screen, so a trust dialog there is the startup one.
func (v hostView) fresh() bool {
	if v.out >= 0 {
		return v.out <= hostPromptMaxOutBytes
	}
	return v.lines <= hostPromptMaxLines
}

// readHostView takes one screen read (and a ping only when a dialog shows) with short timeouts.
func readHostView(h *HostInfo) (hostView, error) {
	r, err := hostCallWithin(h, hostRequest{Op: "screen"}, hostViewDial, hostViewTotal)
	if err != nil {
		return hostView{}, err
	}
	all := strings.Split(r.Text, "\n")
	v := hostView{out: -1}
	for _, l := range all {
		if strings.TrimSpace(l) != "" {
			v.lines++
		}
	}
	if r.Rows > 0 && len(all) > r.Rows {
		all = all[len(all)-r.Rows:]
	}
	v.screen = strings.Join(all, "\n")
	if detectStartPrompt(v.screen, hostAgentName(h)) != nil {
		if p, err := hostCallWithin(h, hostRequest{Op: "ping"}, hostViewDial, hostViewTotal); err == nil {
			v.out = p.OutBytes
		}
	}
	return v, nil
}

// cachedHostView serves host list requests from a read at most hostViewTTL old.
func cachedHostView(h *HostInfo) (hostView, error) {
	hostViewMu.Lock()
	e, ok := hostViews[h.HostID]
	hostViewMu.Unlock()
	if ok && time.Since(e.at) < hostViewTTL {
		return e.v, e.err
	}
	v, err := readHostView(h)
	hostViewMu.Lock()
	hostViews[h.HostID] = hostViewEntry{at: time.Now(), v: v, err: err}
	hostViewMu.Unlock()
	return v, err
}

// pruneHostPromptState drops answer marks and cached reads of hosts that are gone.
func pruneHostPromptState(live []HostInfo) {
	ids := make(map[string]bool, len(live))
	for _, h := range live {
		ids[h.HostID] = true
	}
	hostAnswerMu.Lock()
	for id := range hostAnsweredAt {
		if !ids[id] {
			delete(hostAnsweredAt, id)
		}
	}
	hostAnswerMu.Unlock()
	hostViewMu.Lock()
	for id := range hostViews {
		if !ids[id] {
			delete(hostViews, id)
		}
	}
	hostViewMu.Unlock()
}

// withStartState adds the trust prompt and screen tail to a host that has no session yet.
func withStartState(h *HostInfo) {
	if h == nil || h.AgentID != "" {
		return
	}
	v, err := cachedHostView(h)
	if err != nil {
		return
	}
	h.ScreenTail = screenTail(v.screen, hostScreenTailLines)
	if recentlyAnswered(h.HostID) || !v.fresh() {
		return
	}
	if p := detectStartPrompt(v.screen, hostAgentName(h)); p != nil {
		p.Folder = h.Cwd
		h.Prompt = p
	}
}

func withStartStates(list []HostInfo) {
	var wg sync.WaitGroup
	for i := range list {
		if list[i].AgentID != "" {
			continue
		}
		wg.Add(1)
		go func(h *HostInfo) {
			defer wg.Done()
			withStartState(h)
		}(&list[i])
	}
	wg.Wait()
}

var errNoHostPrompt = errors.New("no prompt on the host screen")

// hostAnswer says what an answer sent: the option confirmed, or none when Escape dismissed the dialog.
type hostAnswer struct {
	Option string
	Key    string
}

func currentHostPrompt(h *HostInfo) (*hostPrompt, error) {
	v, err := readHostView(h)
	if err != nil {
		return nil, err
	}
	if !v.fresh() {
		return nil, errNoHostPrompt
	}
	return detectStartPrompt(v.screen, hostAgentName(h)), nil
}

// answerHostPrompt moves the cursor to the chosen option, checks the screen, then confirms with Enter.
func answerHostPrompt(h *HostInfo, option string) (hostAnswer, error) {
	if h.AgentID != "" {
		return hostAnswer{}, errNoHostPrompt
	}
	p, err := currentHostPrompt(h)
	if err != nil {
		return hostAnswer{}, err
	}
	if p == nil {
		return hostAnswer{}, errNoHostPrompt
	}
	if option == "cancel" {
		if p.index("deny") < 0 {
			return hostAnswer{Key: "Escape"}, markAnswered(h, hostSendKeys(h, []string{"Escape"}))
		}
		option = "deny"
	}
	target := p.index(option)
	if target < 0 {
		return hostAnswer{}, errTerminalBadRequest{"unknown option " + option}
	}
	done := hostAnswer{Option: option, Key: "Enter"}
	if p.cursor < 0 {
		if p.digits[target] == 0 {
			return hostAnswer{}, errors.New("the prompt shows no cursor and no option numbers")
		}
		return done, markAnswered(h, hostSendKeys(h, []string{string(rune('0' + p.digits[target]%10)), "Enter"}))
	}
	if d := target - p.cursor; d != 0 {
		key := "Down"
		if d < 0 {
			key, d = "Up", -d
		}
		keys := make([]string, d)
		for i := range keys {
			keys[i] = key
		}
		if err := hostSendKeys(h, keys); err != nil {
			return hostAnswer{}, err
		}
		time.Sleep(hostAnswerSettle)
		q, err := currentHostPrompt(h)
		if err != nil && !errors.Is(err, errNoHostPrompt) {
			return hostAnswer{}, err
		}
		if q == nil || q.cursor != target || q.Options[target].Label != p.Options[target].Label {
			return hostAnswer{}, errors.New("the cursor did not reach the option; nothing was confirmed")
		}
	}
	return done, markAnswered(h, hostSendKeys(h, []string{"Enter"}))
}

func markAnswered(h *HostInfo, err error) error {
	hostViewMu.Lock()
	delete(hostViews, h.HostID)
	hostViewMu.Unlock()
	if err == nil {
		hostAnswerMu.Lock()
		hostAnsweredAt[h.HostID] = time.Now()
		hostAnswerMu.Unlock()
	}
	return err
}

func serveHostAnswer(w http.ResponseWriter, r *http.Request) {
	var h *HostInfo
	if hostRegistry != nil {
		h = hostRegistry.Get(r.PathValue("id"))
	}
	if h == nil {
		writeJSONStatus(w, http.StatusNotFound, map[string]any{"error": "host not found"})
		return
	}
	var in struct {
		Option string `json:"option"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&in); err != nil || in.Option == "" {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": "option required"})
		return
	}
	hostAnswerMu.Lock()
	_, busy := hostAnswering[h.HostID]
	if !busy {
		hostAnswering[h.HostID] = struct{}{}
	}
	hostAnswerMu.Unlock()
	if busy {
		writeJSONStatus(w, http.StatusConflict, map[string]any{"error": "an answer is already being sent"})
		return
	}
	defer func() {
		hostAnswerMu.Lock()
		delete(hostAnswering, h.HostID)
		hostAnswerMu.Unlock()
	}()
	sent, err := answerHostPrompt(h, in.Option)
	switch {
	case err == nil:
		out := map[string]any{"ok": true, "key": sent.Key}
		if sent.Option != "" {
			out["option"] = sent.Option
		}
		writeJSON(w, out)
	case errors.Is(err, errNoHostPrompt):
		writeJSONStatus(w, http.StatusConflict, map[string]any{"error": err.Error()})
	case errors.As(err, new(errTerminalBadRequest)):
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
	default:
		writeJSONStatus(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
	}
}
