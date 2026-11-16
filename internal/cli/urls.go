// urls — готовые ссылки с токеном при старте (по требованию UX: из лога
// запуска сразу заходят в админку). Печатаем loopback + все интерфейсы,
// если слушатель — «все» (":8085", 0.0.0.0, [::], [*]); конкретный host —
// только он.
package cli

import (
	"fmt"
	"net"
	"sort"
	"strings"
)

// listenHosts — хосты для печати: loopback, потом IP-адреса интерфейсов.
func listenHosts(listen string) ([]string, bool) {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return nil, false
	}
	switch host {
	case "", "0.0.0.0", ":::", "*", "[*]", "::":
		// loopback первым, затем IP интерфейсов по возрастанию
		return append([]string{"127.0.0.1"}, ipsOfInterfaces()...), true
	}
	return []string{host}, false
}

// ipsOfInterfaces — все IP хоста: IPv4 (не loopback/link-local), затем
// глобальные IPv6 (2000::/3 и ULA fd00::/8). Отсортированы для стабильного
// вывода.
func ipsOfInterfaces() []string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	var v4, v6 []string
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip := ipnet.IP
		switch {
		case ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified():
			continue
		case ip.To4() != nil:
			v4 = append(v4, ip.String())
		case ip.IsGlobalUnicast():
			// IPv6: глобальные и ULA; fe80::/10 отсеялся IsLinkLocalUnicast
			v6 = append(v6, ip.String())
		}
	}
	sort.Slice(v4, func(i, j int) bool { return ipv4Less(v4[i], v4[j]) })
	sort.Strings(v6)
	return append(v4, v6...)
}

// ipv4Less — числовое сравнение октетов (lex-строки не годятся: 10 < 9).
func ipv4Less(a, b string) bool {
	ao := strings.Split(a, ".")
	bo := strings.Split(b, ".")
	if len(ao) != 4 || len(bo) != 4 {
		return a < b
	}
	for i := 0; i < 4; i++ {
		var x, y int
		fmt.Sscanf(ao[i], "%d", &x)
		fmt.Sscanf(bo[i], "%d", &y)
		if x != y {
			return x < y
		}
	}
	return false
}

// urlHost — хост для URL: IPv6 оборачивается в [].
func urlHost(h string) string {
	if strings.Contains(h, ":") {
		return "[" + h + "]"
	}
	return h
}

// adminURLs — ссылки админки с токеном по каждому хосту слушателя.
func adminURLs(listen, token string) []string {
	hosts, _ := listenHosts(listen)
	urls := make([]string, 0, len(hosts))
	for _, h := range hosts {
		urls = append(urls, fmt.Sprintf("http://%s:%s/admin?token=%s", urlHost(h), portOf(listen), token))
	}
	return urls
}

// v1URLs — подсказка клиентам OpenAI-совместимым: базовые ссылки /v1.
func v1URLs(listen string) []string {
	hosts, _ := listenHosts(listen)
	urls := make([]string, 0, len(hosts))
	for _, h := range hosts {
		urls = append(urls, fmt.Sprintf("http://%s:%s/v1", urlHost(h), portOf(listen)))
	}
	return urls
}

// portOf — порт слушателя (запасной 8085 — если парсер молчит).
func portOf(listen string) string {
	_, port, err := net.SplitHostPort(listen)
	if err != nil || port == "" {
		return "8085"
	}
	return port
}
