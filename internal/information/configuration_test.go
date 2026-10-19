package information

import (
	"testing"
	"time"
)

func TestInformationPolicyConfigurationDoesNotLaunchServices(t *testing.T) {
	t.Setenv("GORELAY_INFORMATION_REQUEST_TIMEOUT", "19m")
	t.Setenv("GORELAY_BROWSER_MAX_FAILED_PLANS", "5")
	t.Setenv("GORELAY_BROWSER_ACTION_TIMEOUT", "45s")
	t.Setenv("GORELAY_BROWSER_BINARY", "C:/configuration-only/not-launched.exe")
	s := NewService(nil)
	if err := s.Configure(); err != nil {
		t.Fatal(err)
	}
	if s.RequestTimeout != 19*time.Minute || s.BrowserPolicy.MaxFailedPlans != 5 || s.Browser.ActionTimeout != 45*time.Second {
		t.Fatal("host policy replaced by hard defaults", s.RequestTimeout, s.BrowserPolicy)
	}
	t.Setenv("GORELAY_BROWSER_MAX_FAILED_PLANS", "0")
	if err := s.Configure(); err == nil {
		t.Fatal("unbounded browser attempt policy accepted")
	}
	if s.BrowserPolicy.MaxFailedPlans != 5 {
		t.Fatal("invalid configuration partially mutated valid policy")
	}
}
