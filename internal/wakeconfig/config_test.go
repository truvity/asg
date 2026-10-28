package wakeconfig_test

import (
	"strings"
	"testing"
	"time"

	"github.com/truvity/asg/internal/wakeconfig"
)

const good = `{"region":"eu-west-1","healthPort":8080,"routes":[
 {"name":"arm","listenPort":9001,"selector":{"tagKey":"role","tagValue":"builder-arm"},"targetPort":1234}]}`

func TestDefaults(t *testing.T) {
	c, err := wakeconfig.Parse([]byte(good))
	if err != nil {
		t.Fatal(err)
	}

	if r := c.Routes[0]; r.WakeOn != "data" || r.FirstByteTimeout.D() != 30*time.Second {
		t.Errorf("route defaults: %+v", r)
	}

	if c.IdleTimeout.D() != 20*time.Minute || c.WakeTimeout.D() != 180*time.Second || c.ActivityTag != "wake-proxy/last-active" {
		t.Errorf("defaults: %+v", c)
	}
}

func TestRefusals(t *testing.T) {
	for name, tc := range map[string]struct{ in, want string }{
		"unknown field":  {strings.Replace(good, `"healthPort"`, `"healthPorts"`, 1), "unknown field"},
		"bad duration":   {strings.Replace(good, `"routes"`, `"idleTimeout":"soon","routes"`, 1), "duration"},
		"port clash":     {strings.Replace(good, `"listenPort":9001`, `"listenPort":8080`, 1), "already used"},
		"no routes":      {`{"region":"eu-west-1","healthPort":8080,"routes":[]}`, "at least one route"},
		"bad region":     {strings.Replace(good, "eu-west-1", "mars", 1), "region"},
		"selector=tag":   {strings.Replace(good, `"tagKey":"role"`, `"tagKey":"wake-proxy/last-active"`, 1), "activity tag"},
		"bad wakeOn":     {strings.Replace(good, `"targetPort"`, `"wakeOn":"never","targetPort"`, 1), "wakeOn"},
		"bad first byte": {strings.Replace(good, `"targetPort"`, `"firstByteTimeout":"-1s","targetPort"`, 1), "firstByteTimeout"},
		"trailing":       {good + `{}`, "trailing"},
	} {
		if _, err := wakeconfig.Parse([]byte(tc.in)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err %v, want %q", name, err, tc.want)
		}
	}
}

func TestWakeOnConnectIsKept(t *testing.T) {
	c, err := wakeconfig.Parse([]byte(strings.Replace(good, `"targetPort"`, `"wakeOn":"connect","firstByteTimeout":"5s","targetPort"`, 1)))
	if err != nil {
		t.Fatal(err)
	}

	if r := c.Routes[0]; r.WakeOn != "connect" || r.FirstByteTimeout.D() != 5*time.Second {
		t.Errorf("route: %+v", r)
	}
}
