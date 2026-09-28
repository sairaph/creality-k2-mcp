package discovery

import (
	"context"
	"fmt"
	"time"

	"github.com/sairaph/creality_k2_mcp/internal/crealityws"
	"github.com/sairaph/creality_k2_mcp/internal/moonraker"
)

// Per-step timeouts for the identification sequence (plan-v0.1.0.md
// decision 3). These bound each call independently of whatever internal
// defaults internal/moonraker or internal/crealityws use, so this package's
// own budget stays explicit and stable if those defaults ever change.
const (
	infoTimeout = 2 * time.Second
	wsTimeout   = 3 * time.Second
)

// maxErrLen bounds each per-host error string kept on a Result, so a scan
// across many hosts never accumulates unbounded text from a misbehaving or
// hostile responder.
const maxErrLen = 200

// identify performs the K2 confirmation sequence against one host: GET
// /printer/info and GET /server/info (2s each) through internal/moonraker,
// then K2 confirmation via the port 9999 status push (3s) through
// internal/crealityws (dev_docs/plan-v0.1.0.md decision 3). It is used by
// both Scan (after a host answers the Moonraker port) and ProbeHost (a
// manually entered host, no prior connect check). It never returns an
// error itself: every failure is recorded on the Result so a scan of many
// hosts, or a single manual probe, always gets a usable answer back.
func identify(ctx context.Context, host string, port, wsPort int) Result {
	res := Result{Host: host, Port: port}

	mc := moonraker.New(fmt.Sprintf("http://%s:%d", host, port), "")

	infoCtx, cancel := context.WithTimeout(ctx, infoTimeout)
	info, err := mc.PrinterInfo(infoCtx)
	cancel()
	if err != nil {
		res.Errors = append(res.Errors, shortError("printer/info", err))
	} else {
		res.Hostname = info.Hostname
	}

	serverCtx, cancel := context.WithTimeout(ctx, infoTimeout)
	srv, err := mc.ServerInfo(serverCtx)
	cancel()
	if err != nil {
		res.Errors = append(res.Errors, shortError("server/info", err))
	} else {
		res.APIVersion = srv.APIVersionString
	}

	wc := crealityws.New(host, wsPort)
	wsCtx, cancel := context.WithTimeout(ctx, wsTimeout)
	status, err := wc.ReadStatus(wsCtx)
	cancel()
	switch {
	case err != nil:
		msg := shortError("port 9999", err)
		res.Errors = append(res.Errors, msg)
		res.Reason = "port 9999 did not answer: " + msg
	case status.Model == "":
		res.Reason = "port 9999 answered but reported no model"
	default:
		res.Model = status.Model
		if res.Hostname == "" {
			res.Hostname = status.Hostname
		}
		res.IdentifiedK2 = true
	}

	return res
}

func shortError(step string, err error) string {
	msg := err.Error()
	if len(msg) > maxErrLen {
		msg = msg[:maxErrLen] + "..."
	}
	return step + ": " + msg
}
