package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/yiyiSmallti/precinct-gatehouse/internal/provider"
	sdk "github.com/yiyiSmallti/precinct-gatehouse/plugin"
)

// Unavailable is returned for a provider whose plugin is not running.
func Unavailable(name string) func(context.Context, []byte) (*http.Response, error) {
	return func(context.Context, []byte) (*http.Response, error) {
		return nil, &provider.UpstreamError{Err: fmt.Errorf("provider plugin %s is not running", name), Retryable: true}
	}
}

// ProviderChat adapts a provider plugin to provider.Provider's Chat: the
// streamed messages become an *http.Response body. timeout bounds the whole
// call, like providers[].timeout does for HTTP providers.
func ProviderChat(s *Started, timeout time.Duration) func(context.Context, []byte) (*http.Response, error) {
	pc := s.Impl.(*sdk.ProviderClient)
	return func(ctx context.Context, body []byte) (*http.Response, error) {
		if s.Exited() {
			return nil, &provider.UpstreamError{Err: fmt.Errorf("provider plugin %s exited", s.Name), Retryable: true}
		}
		var head struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &head)
		parent := ctx
		ctx, cancel := context.WithTimeout(ctx, timeout)
		stream, err := pc.Chat(ctx, &sdk.ChatRequest{Model: head.Model, Body: body})
		if err != nil {
			cancel()
			return nil, grpcUpstream(parent, err)
		}
		first, err := stream.Recv()
		if err != nil {
			cancel()
			if errors.Is(err, io.EOF) {
				return nil, &provider.UpstreamError{Err: errors.New("provider plugin returned no response"), Retryable: true}
			}
			return nil, grpcUpstream(parent, err)
		}
		code := int(first.GetStatus())
		if code == 0 {
			code = http.StatusOK
		}
		if code/100 != 2 {
			cancel()
			return nil, &provider.UpstreamError{Status: code, Body: first.GetData(), Retryable: code >= 500 || code == 429 || code == 408}
		}
		pr, pw := io.Pipe()
		go func() {
			defer cancel()
			if len(first.GetData()) > 0 {
				if _, err := pw.Write(first.GetData()); err != nil {
					return
				}
			}
			for {
				m, err := stream.Recv()
				if errors.Is(err, io.EOF) {
					pw.Close()
					return
				}
				if err != nil {
					// only the status code: plugin error text may echo the request
					st, _ := status.FromError(err)
					pw.CloseWithError(fmt.Errorf("provider plugin: %s", st.Code()))
					return
				}
				if _, err := pw.Write(m.GetData()); err != nil {
					return
				}
			}
		}()
		ct := first.GetContentType()
		if ct == "" {
			ct = "application/json"
		}
		return &http.Response{StatusCode: code, Header: http.Header{"Content-Type": {ct}}, Body: pr}, nil
	}
}

func grpcUpstream(ctx context.Context, err error) error {
	st, _ := status.FromError(err)
	if st.Code() == codes.DeadlineExceeded && ctx.Err() == nil {
		return &provider.UpstreamError{Err: fmt.Errorf("provider plugin: timeout"), Retryable: true}
	}
	if ctx.Err() != nil {
		return &provider.UpstreamError{Err: fmt.Errorf("provider plugin: %s", st.Code())}
	}
	switch st.Code() {
	case codes.InvalidArgument:
		return &provider.UpstreamError{Status: http.StatusBadRequest, Body: []byte(`{"error":{"message":"invalid request"}}`)}
	case codes.ResourceExhausted:
		return &provider.UpstreamError{Status: http.StatusTooManyRequests, Retryable: true}
	}
	// only the status code: plugin error text may echo the request
	return &provider.UpstreamError{Err: fmt.Errorf("provider plugin: %s", st.Code()), Retryable: true}
}
