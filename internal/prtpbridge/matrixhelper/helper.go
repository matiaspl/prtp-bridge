package matrixhelper

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"prtp-bridge/internal/prtpbridge/matrix"
)

type Options struct {
	SocketPath      string
	MatrixAddr      string
	MatrixPort      string
	ConnectTimeout  time.Duration
	ExchangeTimeout time.Duration
	Debug           bool
}

type NamesRequest struct {
	Addr       string `json:"addr,omitempty"`
	MatrixPort string `json:"matrix_port,omitempty"`
}

type NamesResponse struct {
	OK    bool                 `json:"ok"`
	Error string               `json:"error,omitempty"`
	Names *matrix.NameSnapshot `json:"names,omitempty"`
}

type CrosspointRequest struct {
	Addr    string `json:"addr,omitempty"`
	XIn     int    `json:"xin"`
	XOut    int    `json:"xout"`
	Enabled bool   `json:"enabled"`
	Save    bool   `json:"save"`
}

type CrosspointResponse struct {
	OK    bool   `json:"ok"`
	Code  string `json:"code,omitempty"`
	Error string `json:"error,omitempty"`
}

type Server struct {
	opts Options
	mu   sync.Mutex
}

func NewServer(opts Options) *Server {
	if opts.ConnectTimeout <= 0 {
		opts.ConnectTimeout = matrix.DefaultConnectTimeout
	}
	if opts.ExchangeTimeout <= 0 {
		opts.ExchangeTimeout = matrix.DefaultExchangeTimeout
	}
	return &Server{opts: opts}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/v1/matrix/names", s.handleNames)
	mux.HandleFunc("/v1/matrix/crosspoint", s.handleCrosspoint)
	return mux
}

func (s *Server) Serve(ctx context.Context) error {
	path := strings.TrimSpace(s.opts.SocketPath)
	if path == "" {
		return errors.New("matrix helper socket path is required")
	}
	if err := prepareSocket(path); err != nil {
		return err
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	defer func() {
		_ = ln.Close()
		_ = os.Remove(path)
	}()

	srv := &http.Server{Handler: s.Handler()}
	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.Serve(ln)
	}()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		if err := <-errCh; err != nil && err != http.ErrServerClosed {
			return err
		}
		return nil
	case err := <-errCh:
		if err == http.ErrServerClosed {
			return nil
		}
		return err
	}
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "method not allowed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleNames(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, NamesResponse{OK: false, Error: "method not allowed"})
		return
	}
	var req NamesRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, NamesResponse{OK: false, Error: "invalid JSON: " + err.Error()})
		return
	}
	addr := matrix.NormalizeAddr(firstNonEmpty(req.Addr, s.opts.MatrixAddr))
	if addr == "" {
		writeJSON(w, http.StatusBadRequest, NamesResponse{OK: false, Error: "matrix address is required"})
		return
	}
	portName := firstNonEmpty(req.MatrixPort, s.opts.MatrixPort)
	target, err := matrix.ParsePortRef(portName)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, NamesResponse{OK: false, Error: err.Error()})
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	snap, err := matrix.FetchNames(addr, target, s.opts.ConnectTimeout, s.opts.ExchangeTimeout, s.opts.Debug)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, NamesResponse{OK: false, Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, NamesResponse{OK: true, Names: snap})
}

func (s *Server) handleCrosspoint(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, CrosspointResponse{OK: false, Error: "method not allowed"})
		return
	}
	var req CrosspointRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, CrosspointResponse{OK: false, Error: "invalid JSON: " + err.Error()})
		return
	}
	addr := matrix.NormalizeAddr(firstNonEmpty(req.Addr, s.opts.MatrixAddr))
	if addr == "" {
		writeJSON(w, http.StatusBadRequest, CrosspointResponse{OK: false, Error: "matrix address is required"})
		return
	}
	if req.XIn < 0 || req.XOut < 0 {
		writeJSON(w, http.StatusBadRequest, CrosspointResponse{OK: false, Error: "xin and xout must be non-negative"})
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	err := matrix.SetCrosspoint(addr, matrix.CrosspointRequest{
		XIn:     req.XIn,
		XOut:    req.XOut,
		Enabled: req.Enabled,
		Save:    req.Save,
	})
	if errors.Is(err, matrix.ErrCrosspointNotImplemented) {
		writeJSON(w, http.StatusNotImplemented, CrosspointResponse{
			OK:    false,
			Code:  "not_implemented",
			Error: "native TCP/2222 single-crosspoint write has not been proven; endpoint is intentionally disabled",
		})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusBadGateway, CrosspointResponse{OK: false, Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, CrosspointResponse{OK: true})
}

type Client struct {
	socketPath string
	http       *http.Client
}

func NewClient(socketPath string) *Client {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socketPath)
		},
	}
	return &Client{socketPath: socketPath, http: &http.Client{Transport: tr, Timeout: 15 * time.Second}}
}

func (c *Client) Health(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://matrix-helper/healthz", nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("helper health returned %s", resp.Status)
	}
	return nil
}

func (c *Client) Names(ctx context.Context, req NamesRequest) (*matrix.NameSnapshot, error) {
	var resp NamesResponse
	status, err := c.post(ctx, "/v1/matrix/names", req, &resp)
	if err != nil {
		return nil, err
	}
	if !resp.OK {
		if resp.Error == "" {
			resp.Error = status
		}
		return nil, errors.New(resp.Error)
	}
	return resp.Names, nil
}

func (c *Client) Crosspoint(ctx context.Context, req CrosspointRequest) (*CrosspointResponse, error) {
	var resp CrosspointResponse
	_, err := c.post(ctx, "/v1/matrix/crosspoint", req, &resp)
	if err != nil {
		return nil, err
	}
	if !resp.OK && resp.Error == "" {
		resp.Error = "matrix helper crosspoint request failed"
	}
	return &resp, nil
}

func (c *Client) post(ctx context.Context, path string, body any, out any) (string, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://matrix-helper"+path, bytes.NewReader(b))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return resp.Status, err
	}
	return resp.Status, nil
}

func prepareSocket(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	if st, err := os.Stat(path); err == nil {
		if st.Mode()&os.ModeSocket == 0 {
			return fmt.Errorf("socket path %s exists and is not a socket", path)
		}
		if err := os.Remove(path); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
