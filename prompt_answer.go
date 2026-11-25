package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// statusRefused is a refusal that leaves the request open (423); 409 means another surface or the terminal answered first.
// Clients keep the card and show the message for 423.
const statusRefused = http.StatusLocked

var rePromptEnd = regexp.MustCompile(`(?i)esc to cancel|enter to confirm|enter to select|enter continue · esc|^\s*❯?\s*[1-9]\.\s`)

// promptKey fingerprints the open prompt: the lines up to its last choice or footer, so the status bar below does not count.
func promptKey(screen string) string {
	if !panePromptOpen(screen) {
		return ""
	}
	lines := strings.Split(lastLines(screen, 40), "\n")
	end := -1
	for i, ln := range lines {
		if rePromptEnd.MatchString(ln) {
			end = i
		}
	}
	if end < 0 {
		return ""
	}
	sum := sha256.Sum256([]byte(compactText(strings.Join(lines[max(0, end-20):end+1], "\n"))))
	return hex.EncodeToString(sum[:6])
}

// promptAnswerKeys picks the keys for "yes" or "no" from the prompt itself; nil when it cannot tell.
func promptAnswerKeys(screen, answer string) []string {
	if answer == "no" {
		return []string{"Escape"}
	}
	if o := claudePermOptions(screen); o != nil {
		return []string{strconv.Itoa(o[0].N)} // a digit selects at once
	}
	if o := codexPermOptions(screen); o != nil && o[0].Key != "" {
		return []string{o[0].Key}
	}
	return cursorMenuKeys(screen, "Yes")
}

var (
	reMenuFooter = regexp.MustCompile(`(?i)enter to (confirm|select)`)
	reMenuCur    = regexp.MustCompile(`^\s{0,2}❯\s+(\S.*?)\s*$`)
	reMenuRow    = regexp.MustCompile(`^\s{3,}(\S.*?)\s*$`)
)

// cursorMenuKeys moves the ❯ of a menu with no numbers (Claude Code's folder trust) to the choice that starts with want.
func cursorMenuKeys(screen, want string) []string {
	lines := strings.Split(lastLines(screen, 24), "\n")
	end := -1
	for i, ln := range lines {
		if reMenuFooter.MatchString(ln) {
			end = i
		}
	}
	if end < 0 {
		return nil
	}
	// Read the choices upward from the footer, then put them in screen order.
	// A choice starts in the column of the label after ❯; wrapped or description lines sit deeper and are skipped.
	type row struct {
		text string
		col  int
	}
	labelCol := func(ln string) int {
		n := 0
		for _, r := range ln {
			if r != ' ' && r != '❯' && r != '\u00a0' {
				break
			}
			n++
		}
		return n
	}
	var all []row
	curFromEnd := -1
	for i := end - 1; i >= 0; i-- {
		ln := lines[i]
		if strings.TrimSpace(ln) == "" && len(all) == 0 {
			continue
		}
		if m := reMenuCur.FindStringSubmatch(ln); m != nil && curFromEnd < 0 {
			curFromEnd = len(all)
			all = append(all, row{m[1], labelCol(ln)})
			continue
		}
		if m := reMenuRow.FindStringSubmatch(ln); m != nil {
			all = append(all, row{m[1], labelCol(ln)})
			continue
		}
		break
	}
	if curFromEnd < 0 {
		return nil
	}
	col := all[curFromEnd].col
	var rows []string
	for k, r := range all {
		if r.col == col {
			if k == curFromEnd {
				curFromEnd = len(rows)
			}
			rows = append(rows, r.text)
		}
	}
	slices.Reverse(rows)
	cur := -1
	if curFromEnd >= 0 {
		cur = len(rows) - 1 - curFromEnd
	}
	if cur < 0 || len(rows) < 2 {
		return nil
	}
	for i, r := range rows {
		if reMenuNum.MatchString(r) {
			return nil // a numbered menu takes digits
		}
		if strings.HasPrefix(r, want) {
			var keys []string
			for ; i > cur; i-- {
				keys = append(keys, "Down")
			}
			for ; i < cur; i++ {
				keys = append(keys, "Up")
			}
			return append(keys, "Enter")
		}
	}
	return nil
}

var reMenuNum = regexp.MustCompile(`^[1-9]\.\s`)

// answerPrompt types yes or no into the prompt a card showed, after checking that the screen still shows it.
func answerPrompt(id, answer, key string) (int, error) {
	if answer == "dismiss" {
		return dismissPrompt(id)
	}
	if answer != "yes" && answer != "no" {
		return http.StatusBadRequest, errors.New(`answer must be "yes", "no" or "dismiss"`)
	}
	code := http.StatusOK
	_, rcode, err := withTerminal(id, func(t string) error {
		scr, err := targetCapture(t, 0, true)
		switch {
		case err != nil:
			code = http.StatusBadGateway
			return err
		case !panePromptOpen(scr):
			code = http.StatusConflict
			return errors.New("the prompt is no longer open")
		case key == "":
			code = statusRefused
			return errors.New("this prompt has no fingerprint; answer it in the terminal")
		case promptKey(scr) != key:
			code = statusRefused
			return errors.New("the terminal shows a different prompt now; check it again")
		}
		keys := promptAnswerKeys(scr, answer)
		if keys == nil {
			code = statusRefused
			return errors.New("agent-monitor cannot read the choices of this prompt; answer it in the terminal")
		}
		return targetKeys(t, keys)
	})
	if err != nil {
		if code == http.StatusOK {
			code = rcode
		}
		return code, err
	}
	return http.StatusOK, nil
}

// dismissPrompt clears a prompt card the user answered elsewhere; a prompt still on a watched screen comes back.
func dismissPrompt(id string) (int, error) {
	if store == nil {
		return http.StatusServiceUnavailable, errors.New("no session store")
	}
	s := store.Get(id)
	if s == nil {
		return http.StatusNotFound, errors.New("session not found")
	}
	if s.State == StateAwaitingPermission { // answered in the terminal: the turn goes on until Stop
		store.SetState(s.Tool, s.SessionID, StateRunning, "")
	}
	return http.StatusOK, nil
}

// POST /api/session/prompt/{id}: {answer: "yes"|"no"|"dismiss", key: promptKey from the needs-you item}.
func registerPromptAnswerRoute(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/session/prompt/{id}", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Answer string `json:"answer"`
			Key    string `json:"key"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&in); err != nil {
			writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": "bad json"})
			return
		}
		if code, err := answerPrompt(r.PathValue("id"), in.Answer, in.Key); err != nil {
			writeJSONStatus(w, code, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, map[string]any{"ok": true})
	})
}
