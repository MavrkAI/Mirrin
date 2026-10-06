//go:build windows

package browser

import "golang.org/x/sys/windows/registry"

// windowsProxy reads the proxy set in Internet Options for this user (the
// settings Chrome used to follow on Windows).
func windowsProxy() *sysProxy {
	k, err := registry.OpenKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Internet Settings`, registry.QUERY_VALUE)
	if err != nil {
		return nil
	}
	defer k.Close()
	enable, _, _ := k.GetIntegerValue("ProxyEnable")
	server, _, _ := k.GetStringValue("ProxyServer")
	override, _, _ := k.GetStringValue("ProxyOverride")
	pac, _, _ := k.GetStringValue("AutoConfigURL")
	return parseWindowsProxy(enable == 1, server, override, pac)
}
