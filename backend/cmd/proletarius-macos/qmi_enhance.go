package main

import (
	"context"
	"time"

	"github.com/iniwex5/quectel-qmi-go/pkg/manager"
	"github.com/iniwex5/quectel-qmi-go/pkg/qmi"
	"proletarius.local/core/internal/backend"
)

// qmiEnhancement 是在 AT 主链路之外可选接入的 QMI 增强通道。
//
// 设计目标：完全不影响现有 AT 功能。QMI 通道仅用于提供 AT 链路难以覆盖的
// 增强能力（VoWiFi 状态查询、IMS 注册状态、IMS 服务设置等）。
// QMI 通道不可用时，AT 主链路继续正常工作。
type qmiEnhancement struct {
	mgr  *manager.Manager
	src  backend.QMISource
	path string
}

// startQMIEnhancement 打开 QMI 通道并启动 manager core。
func startQMIEnhancement(path string) (*qmiEnhancement, error) {
	return startQMIEnhancementContext(context.Background(), path)
}

func startQMIEnhancementContext(ctx context.Context, path string) (*qmiEnhancement, error) {
	cfg := manager.Config{
		Device: manager.ModemDevice{
			ControlPath: path,
		},
		NoDial:          true,
		DataPlanePolicy: manager.DataPlanePolicyDisabled,
	}
	mgr := manager.New(cfg, nil)
	if err := mgr.StartCoreContext(ctx); err != nil {
		return nil, err
	}
	src := backend.NewQMISourceAdapter(mgr)
	return &qmiEnhancement{
		mgr:  mgr,
		src:  src,
		path: path,
	}, nil
}

func (e *qmiEnhancement) Close() error {
	if e == nil || e.mgr == nil {
		return nil
	}
	return e.mgr.Stop()
}

// ---- QMI 探测回调 ----

// qmiProbe 返回 IMS 服务设置 + IMS 注册状态查询回调。
func (e *qmiEnhancement) qmiProbe() backend.QMIProbeFunc {
	if e == nil || e.mgr == nil {
		return nil
	}
	return func(ctx context.Context) (*qmi.IMSServicesEnabledSettings, *qmi.IMSARegistrationStatus, error) {
		settings, err := e.mgr.IMSGetServicesEnabledSetting(ctx)
		if err != nil {
			return nil, nil, err
		}
		reg, err := e.mgr.IMSAGetIMSRegistrationStatus(ctx)
		if err != nil {
			return settings, nil, err
		}
		return settings, reg, nil
	}
}

// qmiSetIMS 返回 QMI IMS 服务设置回调。
func (e *qmiEnhancement) qmiSetIMS() backend.QMISetFunc {
	if e == nil || e.mgr == nil {
		return nil
	}
	return func(ctx context.Context, update qmi.IMSServicesEnabledSettingsUpdate) error {
		return e.mgr.IMSSetServicesEnabledSetting(ctx, update)
	}
}

// qmiOperator 返回运营商信息查询回调（MCC, MNC, 运营商名称）。
func (e *qmiEnhancement) qmiOperator() backend.QMIOperatorFunc {
	if e == nil || e.mgr == nil {
		return nil
	}
	return func(ctx context.Context) (mcc, mnc, name string, err error) {
		// 先尝试从 QMI 获取原生 MCC/MNC
		mcc, mnc, err = e.mgr.GetNativeMCCMNC(ctx)
		if err != nil {
			return "", "", "", err
		}
		// 尝试获取运营商名称
		opInfo, opErr := e.mgr.NASGetOperatorName(ctx)
		if opErr == nil && opInfo != nil {
			name = opInfo.ServiceProviderName
			if name == "" {
				name = opInfo.OperatorStringName
			}
		}
		return mcc, mnc, name, nil
	}
}

// ---- app 实现 backend.VoWiFiATProvider ----

// RunCommand 实现 backend.VoWiFiATProvider；timeoutMs 毫秒。
func (a *app) RunCommand(command string, timeoutMs int) (string, error) {
	if timeoutMs <= 0 {
		timeoutMs = 5000
	}
	return a.runATCommand(command, time.Duration(timeoutMs)*time.Millisecond)
}

func (a *app) qmiProbe() backend.QMIProbeFunc {
	if a == nil || a.qmiEnh == nil {
		return nil
	}
	return a.qmiEnh.qmiProbe()
}

func (a *app) qmiSetIMS() backend.QMISetFunc {
	if a == nil || a.qmiEnh == nil {
		return nil
	}
	return a.qmiEnh.qmiSetIMS()
}

func (a *app) qmiOperator() backend.QMIOperatorFunc {
	if a == nil || a.qmiEnh == nil {
		return nil
	}
	return a.qmiEnh.qmiOperator()
}
