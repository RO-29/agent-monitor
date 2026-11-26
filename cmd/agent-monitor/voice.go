package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// POST /api/voice/clean pipes dictated text through ~/voice-dictation/clean.sh; GET /api/voice/status reports it.

const (
	voiceMaxInput = 8 << 10
	voiceSlotWait = 5 * time.Second
)

var voiceTimeout = 20 * time.Second // tests shorten it

var (
	voiceSlots  = make(chan struct{}, 2)
	voiceModeRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)
)

// voiceScript is the cleaner path; AM_VOICE_CLEAN overrides it.
func voiceScript() string {
	if p := os.Getenv("AM_VOICE_CLEAN"); p != "" {
		return p
	}
	return filepath.Join(homeDir(), "voice-dictation", "clean.sh")
}

// voiceConf is the plain dictation config; AM_VOICE_CONF overrides it.
func voiceConf() string {
	if p := os.Getenv("AM_VOICE_CONF"); p != "" {
		return p
	}
	return filepath.Join(homeDir(), ".config", "voice-dictation", "dictate.conf")
}

// voiceEngine reads only the last engine= line of dictate.conf, with clean.sh's default.
func voiceEngine() string {
	engine := ""
	if f, err := os.Open(voiceConf()); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			if v, ok := strings.CutPrefix(sc.Text(), "engine="); ok {
				engine = strings.TrimSpace(v)
			}
		}
		f.Close()
	}
	if engine == "" {
		engine = "claude"
	}
	return engine
}

func voiceAvailable() bool {
	st, err := os.Stat(voiceScript())
	return err == nil && st.Mode().IsRegular()
}

type voiceCleanResp struct {
	Text    string `json:"text"`
	Cleaned bool   `json:"cleaned"`
	Ms      int64  `json:"ms"`
	Error   string `json:"error,omitempty"`
}

// runVoiceClean execs the script with bash (no shell string) and returns the cleaned text or an error.
func runVoiceClean(ctx context.Context, text, mode string) (string, error) {
	script := voiceScript()
	if !voiceAvailable() {
		return "", errors.New("voice cleaner not found: " + script)
	}
	ctx, cancel := context.WithTimeout(ctx, voiceTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/bash", script)
	cmd.Stdin = strings.NewReader(text)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &limitedBuffer{buf: &out, max: 256 << 10}, &limitedBuffer{buf: &errb, max: 4096}
	cmd.Env = os.Environ()
	if mode != "" {
		cmd.Env = append(cmd.Env, "DICTATE_MODE="+mode)
	}
	// The script starts its own children; kill the whole group on timeout so pipes close.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 2 * time.Second
	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return "", fmt.Errorf("voice cleaner timed out after %v", voiceTimeout)
	}
	if err != nil {
		msg := "voice cleaner failed: " + err.Error()
		if s := strings.TrimSpace(errb.String()); s != "" {
			msg += ": " + lastLine(s)
		}
		return "", errors.New(msg)
	}
	clean := strings.TrimSpace(out.String())
	if clean == "" {
		return "", errors.New("voice cleaner returned no text")
	}
	return clean, nil
}

func lastLine(s string) string {
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	if len(s) > 300 {
		s = s[:300]
	}
	return s
}

// limitedBuffer keeps the first max bytes and drops the rest.
type limitedBuffer struct {
	buf *bytes.Buffer
	max int
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if room := l.max - l.buf.Len(); room > 0 {
		l.buf.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

func handleVoiceClean(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text string `json:"text"`
		Mode string `json:"mode"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON body"})
		return
	}
	if len(body.Text) > voiceMaxInput {
		writeJSONStatus(w, http.StatusRequestEntityTooLarge, map[string]any{"error": "text is larger than 8 KB"})
		return
	}
	if body.Mode != "" && !voiceModeRe.MatchString(body.Mode) {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": "mode must be 1-32 letters, digits, _ or -"})
		return
	}
	start := time.Now()
	resp := voiceCleanResp{Text: body.Text}
	if strings.TrimSpace(body.Text) == "" {
		writeJSON(w, resp)
		return
	}
	t := time.NewTimer(voiceSlotWait)
	defer t.Stop()
	select {
	case voiceSlots <- struct{}{}:
		clean, err := runVoiceClean(r.Context(), body.Text, body.Mode)
		<-voiceSlots
		if err != nil {
			resp.Error = err.Error()
		} else {
			resp.Text, resp.Cleaned = clean, true
		}
	case <-t.C:
		resp.Error = "voice cleaner busy: 2 requests are already running"
	case <-r.Context().Done():
		return
	}
	resp.Ms = time.Since(start).Milliseconds()
	writeJSON(w, resp)
}

func registerVoiceRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/voice/clean", handleVoiceClean)
	mux.HandleFunc("GET /api/voice/status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"available": voiceAvailable(), "engine": voiceEngine()})
	})
}
