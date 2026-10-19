package aiapp

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestGinErrorsDistinguishBalanceSystemAndModelRepair(t *testing.T) {
	for _, tc := range []struct {
		name            string
		err             error
		owner, strategy string
	}{
		{"balance", ErrProviderQuota, "business", "stop_and_notify_on_deployment"},
		{"provider-unavailable", &ProviderError{Status: 503, Retryable: true}, "system", "retry_dependency_then_log"},
		{"lua-error", &RunnerError{Code: "lua_error", Message: "unexpected symbol"}, "agent", "correct_and_retry"},
		{"parameter-error", &ParameterError{Reason: "required field missing"}, "agent", "correct_and_retry"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			out := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(out)
			apiError(c, tc.err)
			var body struct {
				Status struct {
					Code     int    `json:"code"`
					Owner    string `json:"owner"`
					Strategy string `json:"strategy"`
				} `json:"status"`
			}
			if err := json.Unmarshal(out.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.Status.Code == 0 || body.Status.Owner != tc.owner || body.Status.Strategy != tc.strategy {
				t.Fatalf("internal error ownership missing: %s", out.Body.String())
			}
		})
	}
}
