package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gorilla/websocket"
)

// Publishing while clients disconnect must not send on a closed channel.
func TestWSDisconnectDuringPublish(t *testing.T) {
	t.Setenv("AGENT_MONITOR_HOME", t.TempDir())
	prev := []any{store, permStore, paneRegistry, talkStore}
	t.Cleanup(func() {
		store, permStore, paneRegistry, talkStore = prev[0].(*Store), prev[1].(*PermStore), prev[2].(*PaneRegistry), prev[3].(*TalkStore)
	})
	store, permStore, paneRegistry, talkStore = nil, nil, nil, nil
	if store == nil {
		store = NewStore()
	}
	if permStore == nil {
		permStore = NewPermStore()
	}
	if paneRegistry == nil {
		paneRegistry = NewPaneRegistry()
	}
	if talkStore == nil {
		talkStore = NewTalkStore()
	}
	srv := httptest.NewServer(http.HandlerFunc(handleWS))
	defer srv.Close()
	url := "ws" + strings.TrimPrefix(srv.URL, "http")
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				publishV3("tick", "n", 1)
			}
		}
	}()
	for i := 0; i < 200; i++ {
		c, _, err := websocket.DefaultDialer.Dial(url, nil)
		if err != nil {
			t.Fatal(err)
		}
		c.Close()
	}
	close(stop)
	wg.Wait()
}
