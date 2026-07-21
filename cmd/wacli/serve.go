package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"
	"github.com/steipete/wacli/internal/app"
	"github.com/steipete/wacli/internal/wa"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"rsc.io/qr"
)

// serve runs a long-lived HTTP bridge over one WhatsApp session. The send and
// state routes mirror the Evolution API contract so existing callers (the
// Albert Worker and the VPS watchdog) keep working without changes; /dashboard
// and /qr are wacli-native additions for pairing and monitoring.

type sentRecord struct {
	ID   string    `json:"id"`
	To   string    `json:"to"`
	Ack  string    `json:"ack"`
	Time time.Time `json:"time"`
}

type serveState struct {
	mu      sync.Mutex
	qr      string
	pairing bool
	sent    []sentRecord
}

func (s *serveState) setQR(code string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.qr = code
}

func (s *serveState) snapshotQR() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.qr
}

func (s *serveState) setPairing(active bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pairing = active
	if !active {
		s.qr = ""
	}
}

func (s *serveState) recordSent(id, to string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, sentRecord{ID: id, To: to, Ack: "sent", Time: time.Now().UTC()})
	if len(s.sent) > 200 {
		s.sent = s.sent[len(s.sent)-200:]
	}
}

func (s *serveState) recordAck(ids []types.MessageID, ack string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.sent {
		for _, id := range ids {
			if s.sent[i].ID == string(id) {
				s.sent[i].Ack = ack
			}
		}
	}
}

func (s *serveState) recentSent() []sentRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]sentRecord, len(s.sent))
	copy(out, s.sent)
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

func newServeCmd(flags *rootFlags) *cobra.Command {
	var listen string

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the HTTP bridge (send API + pairing dashboard)",
		Long: "Runs a long-lived HTTP server over one WhatsApp session.\n" +
			"Auth: every route except /healthz and /dashboard requires the apikey\n" +
			"(or x-api-key) header matching the WACLI_SERVE_API_KEY environment variable.",
		RunE: func(cmd *cobra.Command, args []string) error {
			apiKey := strings.TrimSpace(os.Getenv("WACLI_SERVE_API_KEY"))
			if apiKey == "" {
				return fmt.Errorf("WACLI_SERVE_API_KEY is required")
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			a, lk, err := newApp(ctx, flags, true, true)
			if err != nil {
				return err
			}
			defer closeApp(a, lk)
			if err := a.OpenWA(); err != nil {
				return err
			}

			state := &serveState{}

			a.WA().AddEventHandler(func(evt interface{}) {
				if receipt, ok := evt.(*events.Receipt); ok {
					switch receipt.Type {
					case types.ReceiptTypeDelivered:
						state.recordAck(receipt.MessageIDs, "delivered")
					case types.ReceiptTypeRead:
						state.recordAck(receipt.MessageIDs, "read")
					}
				}
			})

			// Connection keeper: pair via QR when needed, otherwise keep the
			// session connected (whatsmeow reconnects on its own once open).
			go func() {
				for ctx.Err() == nil {
					authed := a.WA().IsAuthed()
					state.setPairing(!authed)
					err := a.Connect(ctx, !authed, state.setQR)
					state.setPairing(false)
					if err != nil && ctx.Err() == nil {
						fmt.Fprintf(os.Stderr, "connect: %v (retry in 5s)\n", err)
						time.Sleep(5 * time.Second)
						continue
					}
					// Connected (or paired). Poll until the link drops.
					for ctx.Err() == nil && a.WA().IsConnected() {
						time.Sleep(3 * time.Second)
					}
					time.Sleep(2 * time.Second)
				}
			}()

			mux := buildServeMux(a, state, apiKey)
			server := &http.Server{Addr: listen, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
			fmt.Printf("wacli serve listening on %s\n", listen)
			if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				return err
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&listen, "listen", "127.0.0.1:8088", "listen address")
	return cmd
}

func connectionStateOf(authed, connected, pairing bool) string {
	switch {
	case authed && connected:
		return "open"
	case pairing || connected:
		return "connecting"
	default:
		return "close"
	}
}

func buildServeMux(a *app.App, state *serveState, apiKey string) *http.ServeMux {
	mux := http.NewServeMux()

	authorized := func(r *http.Request) bool {
		got := r.Header.Get("apikey")
		if got == "" {
			got = r.Header.Get("x-api-key")
		}
		return subtle.ConstantTimeCompare([]byte(got), []byte(apiKey)) == 1
	}
	writeJSON := func(w http.ResponseWriter, status int, payload any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(payload)
	}
	guard := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if !authorized(r) {
				writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
				return
			}
			next(w, r)
		}
	}

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})

	mux.HandleFunc("GET /instance/connectionState/{name}", guard(func(w http.ResponseWriter, r *http.Request) {
		state.mu.Lock()
		pairing := state.pairing
		state.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{
			"instance": map[string]any{
				"instanceName": r.PathValue("name"),
				"state":        connectionStateOf(a.WA().IsAuthed(), a.WA().IsConnected(), pairing),
			},
		})
	}))

	mux.HandleFunc("POST /instance/restart/{name}", guard(func(w http.ResponseWriter, r *http.Request) {
		go func() {
			_ = a.WA().ReconnectWithBackoff(context.Background(), time.Second, 30*time.Second)
		}()
		writeJSON(w, http.StatusOK, map[string]any{"status": "restarting"})
	}))

	mux.HandleFunc("POST /chat/whatsappNumbers/{name}", guard(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Numbers []string `json:"numbers"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Numbers) == 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "numbers é obrigatório"})
			return
		}
		results, err := a.WA().IsOnWhatsApp(r.Context(), body.Numbers)
		if err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": err.Error()})
			return
		}
		out := make([]map[string]any, 0, len(results))
		for _, result := range results {
			out = append(out, map[string]any{
				"jid":    result.JID.String(),
				"exists": result.IsIn,
				"number": result.Query,
			})
		}
		writeJSON(w, http.StatusOK, out)
	}))

	mux.HandleFunc("POST /message/sendText/{name}", guard(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Number string `json:"number"`
			To     string `json:"to"`
			Text   string `json:"text"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "JSON inválido"})
			return
		}
		recipient := strings.TrimSpace(body.Number)
		if recipient == "" {
			recipient = strings.TrimSpace(body.To)
		}
		if recipient == "" || strings.TrimSpace(body.Text) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "number e text são obrigatórios"})
			return
		}

		jid, err := resolveRecipient(r.Context(), a.WA(), recipient)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		id, err := a.WA().SendText(r.Context(), jid, body.Text)
		if err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": err.Error()})
			return
		}
		state.recordSent(string(id), jid.String())
		writeJSON(w, http.StatusCreated, map[string]any{
			"key":    map[string]any{"id": string(id), "remoteJid": jid.String(), "fromMe": true},
			"status": "SENT",
		})
	}))

	mux.HandleFunc("GET /qr", guard(func(w http.ResponseWriter, r *http.Request) {
		state.mu.Lock()
		pairing := state.pairing
		qr := state.qr
		state.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{
			"state":   connectionStateOf(a.WA().IsAuthed(), a.WA().IsConnected(), pairing),
			"pairing": pairing,
			"qr":      qr,
		})
	}))

	mux.HandleFunc("GET /messages/recent", guard(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"sent": state.recentSent()})
	}))

	mux.HandleFunc("GET /qr.png", guard(func(w http.ResponseWriter, r *http.Request) {
		code := state.snapshotQR()
		if code == "" {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "sem QR ativo"})
			return
		}
		encoded, err := qr.Encode(code, qr.M)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(encoded.PNG())
	}))

	mux.HandleFunc("GET /dashboard", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(dashboardHTML))
	})

	return mux
}

func resolveRecipient(ctx context.Context, client app.WAClient, recipient string) (types.JID, error) {
	if strings.Contains(recipient, "@") {
		return types.ParseJID(recipient)
	}
	digits := strings.TrimPrefix(recipient, "+")
	results, err := client.IsOnWhatsApp(ctx, []string{digits})
	if err == nil && len(results) > 0 {
		if !results[0].IsIn {
			return types.JID{}, fmt.Errorf("número não está no WhatsApp: %s", digits)
		}
		return results[0].JID.ToNonAD(), nil
	}
	return wa.ParseUserOrJID(digits)
}
