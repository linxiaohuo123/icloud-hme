//go:build linux

// [INPUT]: Production Server.Run, a subprocess, real Linux SIGTERM and temporary SQLite.
// [OUTPUT]: Signal-driven HTTP drain, resource closure and persisted-state recovery regression.
// [POS]: internal/server Linux process lifecycle validation; no external provider calls.
// [PROTOCOL]: Update this header and CLAUDE.md when changing this test.
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"icloud-hme/internal/account"
	"icloud-hme/internal/store"
)

const signalTestAPIKey = "linux-signal-test-api-key"

type signalTestManifest struct {
	Token, PrincipalID string
}

type signalTestReport struct {
	Waiters, Inflight, IMAPConnections, IMAPOperations int
	StoreClosed                                        bool
}

func TestLinuxSIGTERM_HelperProcess(t *testing.T) {
	dir := os.Getenv("ICLOUD_HME_TEST_SIGTERM_DIR")
	if dir == "" {
		t.Skip("subprocess helper")
	}
	st, err := store.NewStore(filepath.Join(dir, "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	mgr, err := account.NewManager(filepath.Join(dir, "db"), st)
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close()
	token, err := st.CreateToken("signal-fixture", store.ScopeVerify, "")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for _, id := range []string{"signal-pending", "signal-completed"} {
		if err := st.CreateVerificationRequest(context.Background(), &store.VerificationRequest{
			RequestID: id, PrincipalKind: "token", PrincipalID: token.ID,
			LeaseID: id + "-lease", AliasEmail: id + "@invalid.example", Status: "ready",
			CreatedAt: now.Format(time.RFC3339), ExpiresAt: now.Add(10 * time.Minute).Format(time.RFC3339),
			BaselineProvider: "imap", BaselineMailbox: "INBOX", BaselineUIDValidity: 1, BaselineUID: 1,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, won, err := st.CompleteVerificationRequestResult(context.Background(), "signal-completed",
		store.VerificationCompletion{Code: "765432", MatchedEventRef: "signal-event"}); err != nil || !won {
		t.Fatalf("prepare completed fixture: won=%v err=%v", won, err)
	}
	s, err := New(mgr, st, Config{AdminPassword: "linux-signal-test-password", APIKey: signalTestAPIKey})
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := json.Marshal(signalTestManifest{Token: token.Token, PrincipalID: token.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.tmp"), manifest, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(dir, "manifest.tmp"), filepath.Join(dir, "manifest.json")); err != nil {
		t.Fatal(err)
	}
	// All background engines start via Run. With zero accounts they cannot contact providers.
	if err := s.Run(os.Getenv("ICLOUD_HME_TEST_SIGTERM_ADDR")); err != nil {
		t.Fatal(err)
	}
	stats := s.requestLimiter.Stats()
	conns, ops, _ := s.be.(interface{ IMAPPoolStats() (int, int, int) }).IMAPPoolStats()
	report := signalTestReport{Waiters: stats.ActiveWaiters, Inflight: stats.ActiveInflight,
		IMAPConnections: conns, IMAPOperations: ops, StoreClosed: st.DB().Ping() != nil}
	if report.Waiters != 0 || report.Inflight != 0 || conns != 0 || ops != 0 || !report.StoreClosed {
		t.Fatalf("Run returned before cleanup: %+v", report)
	}
	data, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "closed.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestLinuxSIGTERM_RunDrainsWaiterAndPreservesStore(t *testing.T) {
	dir := t.TempDir()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestLinuxSIGTERM_HelperProcess$", "-test.timeout=25s")
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, "ICLOUD_HME_") {
			cmd.Env = append(cmd.Env, value)
		}
	}
	cmd.Env = append(cmd.Env, "ICLOUD_HME_TEST_SIGTERM_DIR="+dir, "ICLOUD_HME_TEST_SIGTERM_ADDR="+addr,
		"ICLOUD_HME_MASTER_KEY=MDEyMzQ1Njc4OTAxMjM0NTY3ODkwMTIzNDU2Nzg5MDE=")
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	var processErr error
	go func() { processErr = cmd.Wait(); close(exited) }()
	t.Cleanup(func() {
		select {
		case <-exited:
		default:
			_ = cmd.Process.Kill()
			<-exited
		}
		if t.Failed() {
			t.Log(output.String())
		}
	})
	var manifest signalTestManifest
	until := time.Now().Add(10 * time.Second)
	for {
		data, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
		if err == nil {
			if err := json.Unmarshal(data, &manifest); err != nil {
				t.Fatal(err)
			}
			break
		}
		if !errors.Is(err, os.ErrNotExist) || time.Now().After(until) {
			t.Fatalf("child did not initialize: %v", err)
		}
		select {
		case <-exited:
			t.Fatalf("child exited before readiness: %v", processErr)
		case <-time.After(10 * time.Millisecond):
		}
	}
	client := &http.Client{Timeout: 130 * time.Second}
	t.Cleanup(client.CloseIdleConnections)
	baseURL := "http://" + addr
	for {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		req, _ := http.NewRequestWithContext(ctx, "GET", baseURL+"/readyz", nil)
		resp, err := client.Do(req)
		ready := err == nil && resp.StatusCode == http.StatusOK
		if resp != nil {
			resp.Body.Close()
		}
		cancel()
		if ready {
			break
		}
		if time.Now().After(until) {
			t.Fatalf("child HTTP did not become ready: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	delivered := make(chan error, 1)
	go func() {
		req, _ := http.NewRequestWithContext(ctx, "GET", baseURL+"/api/external/v2/verification-requests/signal-pending?timeout=120", nil)
		req.Header.Set("Authorization", "Bearer "+manifest.Token)
		resp, err := client.Do(req)
		if err != nil {
			delivered <- err
			return
		}
		defer resp.Body.Close()
		var reply capacityReply
		if err := json.NewDecoder(resp.Body).Decode(&reply); err != nil {
			delivered <- err
			return
		}
		if (resp.StatusCode == 503 && reply.Code == "SERVER_SHUTTING_DOWN") ||
			(resp.StatusCode == 499 && reply.Code == "REQUEST_CANCELED") {
			delivered <- nil
		} else {
			delivered <- fmt.Errorf("unexpected shutdown response: status=%d code=%s", resp.StatusCode, reply.Code)
		}
	}()
	until = time.Now().Add(10 * time.Second)
	for {
		statsCtx, stop := context.WithTimeout(context.Background(), time.Second)
		req, _ := http.NewRequestWithContext(statsCtx, "GET", baseURL+"/api/system/stats", nil)
		req.Header.Set("X-API-Key", signalTestAPIKey)
		resp, err := client.Do(req)
		var stats struct {
			Data struct {
				Limits RequestLimiterStats `json:"request_limits"`
			} `json:"data"`
		}
		if err == nil {
			err = json.NewDecoder(resp.Body).Decode(&stats)
			resp.Body.Close()
		}
		stop()
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("read child waiter count: err=%v response=%v", err, resp)
		}
		if stats.Data.Limits.ActiveWaiters == 1 {
			break
		}
		if time.Now().After(until) {
			t.Fatal("child long poll did not enter waiter stage")
		}
		time.Sleep(10 * time.Millisecond)
	}
	started := time.Now()
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-delivered:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(12 * time.Second):
		t.Fatal("SIGTERM did not cancel the accepted HTTP waiter")
	}
	select {
	case <-exited:
		if processErr != nil {
			t.Fatalf("child exited unsuccessfully: %v", processErr)
		}
	case <-time.After(12 * time.Second):
		t.Fatal("SIGTERM did not finish Run")
	}
	var report signalTestReport
	data, err := os.ReadFile(filepath.Join(dir, "closed.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatal(err)
	}
	if report.Waiters != 0 || report.Inflight != 0 || report.IMAPConnections != 0 || report.IMAPOperations != 0 || !report.StoreClosed {
		t.Fatalf("incomplete signal cleanup: %+v", report)
	}
	st, err := store.NewStore(filepath.Join(dir, "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for id, status := range map[string]string{"signal-pending": "ready", "signal-completed": "succeeded"} {
		r, err := st.GetVerificationRequest(context.Background(), id, "token", manifest.PrincipalID)
		if err != nil || r.Status != status || (status == "succeeded" && (r.Code != "765432" || r.MatchedEventRef != "signal-event")) {
			t.Fatalf("state did not survive SIGTERM: request=%+v err=%v", r, err)
		}
	}
	t.Logf("Linux SIGTERM: accepted waiter canceled, Run returned, resources closed, pending/completed state reopened; elapsed=%v", time.Since(started))
}
