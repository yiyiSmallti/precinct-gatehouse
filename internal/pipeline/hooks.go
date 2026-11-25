package pipeline

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"time"

	"github.com/yiyiSmallti/precinct-gatehouse/internal/oai"
	sdk "github.com/yiyiSmallti/precinct-gatehouse/plugin"
)

// HookTimeout bounds one hook call.
const HookTimeout = 5 * time.Second

// Hook is a Hooks plugin. Current returns its running implementation, or
// false while the plugin process is down (it is looked up per call so a
// restarted plugin is picked up).
type Hook struct {
	Name    string
	Current func() (sdk.Hooks, bool)
}

func (p *Pipeline) hookCtx(st *reqState, dataClass string) *sdk.HookContext {
	hc := &sdk.HookContext{RequestId: st.id, DataClass: dataClass, Source: st.rec.Source, User: st.claims.Name}
	if st.org != nil {
		hc.Tenant, hc.User, hc.Department = st.org.Tenant, st.org.User, st.org.Department
	}
	return hc
}

// runRequestHooks lets each hook rewrite req in place. A hook that is not
// running or fails refuses the request (503): hooks may enforce policy, so
// skipping one silently is not safe. A rewrite of model, stream or tools is
// rejected with 500.
func (p *Pipeline) runRequestHooks(ctx context.Context, st *reqState, req oai.Object, dataClass string) error {
	for _, h := range p.Hooks {
		impl, ok := h.Current()
		if !ok {
			return oai.Errorf(http.StatusServiceUnavailable, "hook_unavailable", "hook plugin %s is not running", h.Name)
		}
		cctx, cancel := context.WithTimeout(ctx, HookTimeout)
		res, err := impl.OnRequest(cctx, &sdk.HookRequest{Ctx: p.hookCtx(st, dataClass), Body: oai.Encode(req)})
		cancel()
		if err != nil {
			return oai.Errorf(http.StatusServiceUnavailable, "hook_failed", "hook plugin %s failed", h.Name)
		}
		if len(res.GetBody()) == 0 {
			continue
		}
		nr, err := oai.Decode(res.GetBody())
		if err != nil {
			return oai.Errorf(http.StatusInternalServerError, "hook_invalid", "hook plugin %s returned invalid JSON", h.Name)
		}
		for _, k := range []string{"model", "stream", "tools"} {
			if !reflect.DeepEqual(nr[k], req[k]) {
				return oai.Errorf(http.StatusInternalServerError, "hook_invalid", "hook plugin %s may not change %s", h.Name, k)
			}
		}
		if len(oai.Messages(nr)) == 0 {
			return oai.Errorf(http.StatusInternalServerError, "hook_invalid", "hook plugin %s removed messages", h.Name)
		}
		for k := range req {
			delete(req, k)
		}
		for k, v := range nr {
			req[k] = v
		}
	}
	return nil
}

// runResponseHooks passes the final body to each hook (read-only, best effort).
func (p *Pipeline) runResponseHooks(hc *sdk.HookContext, body []byte, status int) {
	if len(p.Hooks) == 0 {
		return
	}
	for _, h := range p.Hooks {
		impl, ok := h.Current()
		if !ok {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), HookTimeout)
		if err := impl.OnResponse(ctx, &sdk.HookResponse{Ctx: hc, Body: body, Status: int32(status)}); err != nil {
			p.Log.Warn("response hook failed", "hook", h.Name, "err", fmt.Sprint(err))
		}
		cancel()
	}
}
