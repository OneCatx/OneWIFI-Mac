package backend

import (
	"context"

	"github.com/iniwex5/quectel-qmi-go/pkg/qmi"
)

// QMIProbeFunc 是 QMI 辅助查询回调。
type QMIProbeFunc = func(context.Context) (*qmi.IMSServicesEnabledSettings, *qmi.IMSARegistrationStatus, error)

// QMISetFunc 是 QMI IMS 设置回调。
type QMISetFunc = func(context.Context, qmi.IMSServicesEnabledSettingsUpdate) error

// QMIOperatorFunc 是 QMI 运营商信息查询回调（返回 MCC, MNC, 运营商名称）。
type QMIOperatorFunc = func(context.Context) (mcc, mnc, name string, err error)

// VoWiFiATProvider 由 app 层提供 AT 命令执行能力。
type VoWiFiATProvider interface {
	RunCommand(command string, timeoutMs int) (string, error)
}
