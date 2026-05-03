package api

import "strings"

func depositRESTPath() string {
	return "/v1/wallet/deposit"
}

func composeDepositHintURL(controlPlaneOrigin string) string {
	base := strings.TrimRight(strings.TrimSpace(controlPlaneOrigin), "/")
	if base == "" {
		return ""
	}
	return base + depositRESTPath()
}
