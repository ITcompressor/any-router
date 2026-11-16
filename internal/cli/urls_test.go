package cli

import (
	"strings"
	"testing"
)

func TestAdminURLsLoopbackOnly(t *testing.T) {
	urls := adminURLs("127.0.0.1:9000", "tok")
	if len(urls) != 1 {
		t.Fatalf("ссылок: %d, хочу 1 (конкретный host)", len(urls))
	}
	if urls[0] != "http://127.0.0.1:9000/admin?token=tok" {
		t.Errorf("ссылка: %q", urls[0])
	}
}

func TestAdminURLsAllInterfaces(t *testing.T) {
	urls := adminURLs(":8085", "tok")
	// как минимум loopback + по одному на каждый не-loopback интерфейс
	if len(urls) < 2 {
		t.Fatalf("машин с одним loopback не ждём; получили %d: %v", len(urls), urls)
	}
	if urls[0] != "http://127.0.0.1:8085/admin?token=tok" {
		t.Errorf("первая всегда loopback: %q", urls[0])
	}
	for _, u := range urls[1:] {
		if !strings.Contains(u, ":8085/admin?token=tok") {
			t.Errorf("формат ссылки: %q — ждет host:8085/admin?token=tok", u)
		}
	}
	// v1 ссылки — те же хосты, без токена
	v := v1URLs(":8085")
	if len(v) != len(urls) || !strings.HasSuffix(v[0], ":8085/v1") {
		t.Errorf("v1: %v", v)
	}
}

func TestListenHostsExplicitHost(t *testing.T) {
	for _, listen := range []string{"localhost:8085", "10.0.0.1:8085", "[::1]:8085"} {
		hosts, all := listenHosts(listen)
		if all || len(hosts) != 1 {
			t.Errorf("%q: хочу одиночный хост, получили %v all=%v", listen, hosts, all)
		}
	}
	// all-формы
	_, all := listenHosts(":8085")
	if !all {
		t.Errorf(":8085 — все интерфейсы")
	}
	_, all = listenHosts("0.0.0.0:8085")
	if !all {
		t.Errorf("0.0.0.0:8085 — все интерфейсы")
	}
}

func TestIPv4Less(t *testing.T) {
	if !ipv4Less("10.0.0.2", "10.0.0.10") {
		t.Errorf("числовое сравнение сломано: 10.0.0.2 < 10.0.0.10")
	}
	if !ipv4Less("9.0.0.1", "10.0.0.1") {
		t.Errorf("число 9 меньше 10 (строковое сравнение даёт обратное)")
	}
}

func TestPortOf(t *testing.T) {
	for listen, want := range map[string]string{"127.0.0.1:9001": "9001", "": "8085", "хм": "8085"} {
		if got := portOf(listen); got != want {
			t.Errorf("portOf(%q)=%q, хочу %q", listen, got, want)
		}
	}
}

// TestURLHostIPv6 — IPv6 в URL оборачивается в скобки.
func TestURLHostIPv6(t *testing.T) {
	if got := urlHost("::1"); got != "[::1]" {
		t.Errorf("urlHost(::1): %q", got)
	}
	if got := urlHost("192.168.1.1"); got != "192.168.1.1" {
		t.Errorf("urlHost(v4): %q", got)
	}
}
