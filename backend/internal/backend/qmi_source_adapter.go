package backend

import (
	"context"
	"time"

	"github.com/iniwex5/quectel-qmi-go/pkg/manager"
	"github.com/iniwex5/quectel-qmi-go/pkg/qmi"
)

// qmiSourceAdapter 将 *manager.Manager（quectel-qmi-go）包装为 QMISource，
// 使 QMIBackend 可以直接使用 Manager 作为资源池。
//
// manager.Manager 已经实现了 QMISource 的绝大多数方法（device_query.go 等），
// 这里只需要补齐两处签名不一致的方法：
//  1. UIM 逻辑通道方法名映射（OpenEUICCLogicalChannel/CloseEUICCLogicalChannel/TransmitEUICCAPDU
//     → OpenLogicalChannelContext/CloseLogicalChannelContext/SendAPDUContext）
//  2. NASIncrementalNetworkScanSnapshot —— manager 没有暴露增量扫描快照，
//     返回 (nil, zero, false) 表示不可用（上层会优雅降级为全量扫描）。
type qmSourceAdapter struct {
	mgr *manager.Manager
}

// NewQMISourceAdapter 创建 QMISource 适配器。
func NewQMISourceAdapter(mgr *manager.Manager) QMISource {
	if mgr == nil {
		return nil
	}
	return &qmSourceAdapter{mgr: mgr}
}

// ---- QMISource 直接透传（manager.Manager 已实现，签名一致） ----

func (a *qmSourceAdapter) GetDeviceSerialNumbers(ctx context.Context) (*qmi.DeviceInfo, error) {
	return a.mgr.GetDeviceSerialNumbers(ctx)
}

func (a *qmSourceAdapter) GetDeviceRevision(ctx context.Context) (string, string, error) {
	return a.mgr.GetDeviceRevision(ctx)
}

func (a *qmSourceAdapter) GetIMSI(ctx context.Context) (string, error) {
	return a.mgr.GetIMSI(ctx)
}

func (a *qmSourceAdapter) GetICCID(ctx context.Context) (string, error) {
	return a.mgr.GetICCID(ctx)
}

func (a *qmSourceAdapter) GetMSISDN(ctx context.Context) (string, error) {
	return a.mgr.GetMSISDN(ctx)
}

func (a *qmSourceAdapter) GetSIMStatus(ctx context.Context) (qmi.SIMStatus, error) {
	return a.mgr.GetSIMStatus(ctx)
}

func (a *qmSourceAdapter) GetUIMReadiness(ctx context.Context) (manager.UIMReadiness, error) {
	return a.mgr.GetUIMReadiness(ctx)
}

func (a *qmSourceAdapter) GetServingSystem(ctx context.Context) (*qmi.ServingSystem, error) {
	return a.mgr.GetServingSystem(ctx)
}

func (a *qmSourceAdapter) GetSignalStrength(ctx context.Context) (*qmi.SignalStrength, error) {
	return a.mgr.GetSignalStrength(ctx)
}

func (a *qmSourceAdapter) GetSignalInfo(ctx context.Context) (*qmi.SignalInfo, error) {
	return a.mgr.GetSignalInfo(ctx)
}

func (a *qmSourceAdapter) GetSysInfo(ctx context.Context) (*qmi.SysInfo, error) {
	return a.mgr.GetSysInfo(ctx)
}

func (a *qmSourceAdapter) NASGetRFBandInfo(ctx context.Context) (*qmi.RFBandInfo, error) {
	return a.mgr.NASGetRFBandInfo(ctx)
}

func (a *qmSourceAdapter) NASGetCellLocationInfo(ctx context.Context) (*qmi.CellLocationInfo, error) {
	return a.mgr.NASGetCellLocationInfo(ctx)
}

func (a *qmSourceAdapter) GetOperatingMode(ctx context.Context) (qmi.OperatingMode, error) {
	return a.mgr.GetOperatingMode(ctx)
}

func (a *qmSourceAdapter) SetOperatingMode(ctx context.Context, mode qmi.OperatingMode) error {
	return a.mgr.SetOperatingMode(ctx, mode)
}

func (a *qmSourceAdapter) WMSSendRawMessage(ctx context.Context, format uint8, pdu []byte) error {
	return a.mgr.WMSSendRawMessage(ctx, format, pdu)
}

func (a *qmSourceAdapter) WMSRawReadMessage(ctx context.Context, storageType uint8, index uint32) ([]byte, error) {
	return a.mgr.WMSRawReadMessage(ctx, storageType, index)
}

func (a *qmSourceAdapter) WMSDeleteMessage(ctx context.Context, storageType uint8, index uint32) error {
	return a.mgr.WMSDeleteMessage(ctx, storageType, index)
}

func (a *qmSourceAdapter) WMSListMessagesAuto(ctx context.Context, storageType uint8) ([]struct {
	Index uint32
	Tag   qmi.MessageTagType
}, error) {
	return a.mgr.WMSListMessagesAuto(ctx, storageType)
}

func (a *qmSourceAdapter) WMSDeleteMessagesByTag(ctx context.Context, storageType uint8, tag qmi.MessageTagType, mode qmi.MessageMode) error {
	return a.mgr.WMSDeleteMessagesByTag(ctx, storageType, tag, mode)
}

// ---- UIM 逻辑通道：方法名映射到 manager 的实现 ----

func (a *qmSourceAdapter) OpenEUICCLogicalChannel(ctx context.Context, slot byte, aid []byte) (byte, error) {
	return a.mgr.OpenLogicalChannelContext(ctx, slot, aid)
}

func (a *qmSourceAdapter) CloseEUICCLogicalChannel(ctx context.Context, slot byte, channel byte) error {
	return a.mgr.CloseLogicalChannelContext(ctx, slot, channel)
}

func (a *qmSourceAdapter) TransmitEUICCAPDU(ctx context.Context, slot byte, channel byte, command []byte) ([]byte, error) {
	return a.mgr.SendAPDUContext(ctx, slot, channel, command)
}

func (a *qmSourceAdapter) UIMPowerOffSIM(ctx context.Context, slot uint8) error {
	return a.mgr.UIMPowerOffSIM(ctx, slot)
}

func (a *qmSourceAdapter) UIMPowerOnSIM(ctx context.Context, slot uint8) error {
	return a.mgr.UIMPowerOnSIM(ctx, slot)
}

func (a *qmSourceAdapter) UIMPostSwitchReload(ctx context.Context, readiness manager.UIMReadiness, opts manager.UIMPostSwitchReloadOptions) (uint8, error) {
	return a.mgr.UIMPostSwitchReload(ctx, readiness, opts)
}

func (a *qmSourceAdapter) EnsureSIMProvisioned(ctx context.Context, opts manager.EnsureSIMProvisionedOptions) (manager.UIMReadiness, error) {
	return a.mgr.EnsureSIMProvisioned(ctx, opts)
}

func (a *qmSourceAdapter) GetNativeMCCMNC(ctx context.Context) (mcc, mnc string, err error) {
	return a.mgr.GetNativeMCCMNC(ctx)
}

func (a *qmSourceAdapter) GetNativeSPN(ctx context.Context) (string, error) {
	return a.mgr.GetNativeSPN(ctx)
}

func (a *qmSourceAdapter) GetSIMMetadata(ctx context.Context) (*qmi.SIMMetadata, error) {
	return a.mgr.GetSIMMetadata(ctx)
}

func (a *qmSourceAdapter) GetSMSC(ctx context.Context) (string, error) {
	return a.mgr.GetSMSC(ctx)
}

func (a *qmSourceAdapter) GetDeviceSnapshot() *manager.DeviceSnapshot {
	return a.mgr.GetDeviceSnapshot()
}

// ---- 可选接口实现 ----

// strictLive：QMIBackend.GetIMSILive 需要
func (a *qmSourceAdapter) GetIMSIStrictLive(ctx context.Context) (string, error) {
	return a.mgr.GetIMSIStrictLive(ctx)
}

// qmiEFADReader
func (a *qmSourceAdapter) UIMReadTransparentWithSession(ctx context.Context, sessionType uint8, fileID uint16, path []uint8) ([]byte, error) {
	return a.mgr.UIMReadTransparentWithSession(ctx, sessionType, fileID, path)
}

// qmiSIMAuthLogicalChannelSource
func (a *qmSourceAdapter) OpenSIMAuthLogicalChannel(ctx context.Context, slot byte, aid []byte) (byte, error) {
	return a.mgr.OpenLogicalChannelContext(ctx, slot, aid)
}

func (a *qmSourceAdapter) CloseSIMAuthLogicalChannel(ctx context.Context, slot byte, channel byte) error {
	return a.mgr.CloseLogicalChannelContext(ctx, slot, channel)
}

// qmiSIMAuthAIDSource
func (a *qmSourceAdapter) GetUSIMAID(ctx context.Context) ([]byte, error) {
	return a.mgr.GetUSIMAID(ctx)
}

func (a *qmSourceAdapter) GetISIMAID(ctx context.Context) ([]byte, error) {
	return a.mgr.GetISIMAID(ctx)
}

// nasControlSource —— manager 已实现除 NASIncrementalNetworkScanSnapshot 外全部方法
func (a *qmSourceAdapter) NASInitiateNetworkRegister(ctx context.Context, req qmi.NASInitiateNetworkRegisterRequest) error {
	return a.mgr.NASInitiateNetworkRegister(ctx, req)
}

func (a *qmSourceAdapter) NASForceNetworkSearch(ctx context.Context) error {
	return a.mgr.NASForceNetworkSearch(ctx)
}

func (a *qmSourceAdapter) NASAttachDetach(ctx context.Context, attached bool) error {
	return a.mgr.NASAttachDetach(ctx, attached)
}

func (a *qmSourceAdapter) NASSetSystemSelectionPreference(ctx context.Context, pref qmi.SystemSelectionPreference) error {
	return a.mgr.NASSetSystemSelectionPreference(ctx, pref)
}

func (a *qmSourceAdapter) NASGetOperatorName(ctx context.Context) (*qmi.NASOperatorNameInfo, error) {
	return a.mgr.NASGetOperatorName(ctx)
}

func (a *qmSourceAdapter) NASGetPLMNName(ctx context.Context, req qmi.NASPLMNNameRequest) (*qmi.NASPLMNNameInfo, error) {
	return a.mgr.NASGetPLMNName(ctx, req)
}

func (a *qmSourceAdapter) NASConfigSignalInfoV2(ctx context.Context, cfg qmi.NASSignalInfoConfigV2) error {
	return a.mgr.NASConfigSignalInfoV2(ctx, cfg)
}

func (a *qmSourceAdapter) NASRegisterIndications(ctx context.Context, cfg qmi.NASIndicationRegistration) error {
	return a.mgr.NASRegisterIndications(ctx, cfg)
}

func (a *qmSourceAdapter) NASPerformNetworkScan(ctx context.Context) ([]qmi.NetworkScanResult, error) {
	return a.mgr.NASPerformNetworkScan(ctx)
}

// NASIncrementalNetworkScanSnapshot —— manager 未实现，返回不可用。
func (a *qmSourceAdapter) NASIncrementalNetworkScanSnapshot() (*qmi.NASIncrementalNetworkScanInfo, time.Time, bool) {
	return nil, time.Time{}, false
}

func (a *qmSourceAdapter) NASGetSystemSelectionPreference(ctx context.Context) (*qmi.SystemSelectionPreference, error) {
	return a.mgr.NASGetSystemSelectionPreference(ctx)
}

// coreRecoveryRequester
func (a *qmSourceAdapter) RequestCoreRecovery(reason string) bool {
	return a.mgr.RequestCoreRecovery(reason)
}

// coreReadyWaiter
func (a *qmSourceAdapter) WaitCoreReady(ctx context.Context) error {
	return a.mgr.WaitCoreReady(ctx)
}

// 编译期断言：qmSourceAdapter 必须满足全部可选接口。
// 注：QMIBackend 内部以函数内匿名接口（coreReadyWaiter/coreRecoveryRequester）断言，
// 这里用同签名的具名接口保证实现不被破坏。
type qmiCoreReadyWaiter interface {
	WaitCoreReady(ctx context.Context) error
}

type qmiCoreRecoveryRequester interface {
	RequestCoreRecovery(reason string) bool
}

var (
	_ QMISource                      = (*qmSourceAdapter)(nil)
	_ nasControlSource               = (*qmSourceAdapter)(nil)
	_ qmiSIMAuthLogicalChannelSource = (*qmSourceAdapter)(nil)
	_ qmiSIMAuthAIDSource            = (*qmSourceAdapter)(nil)
	_ qmiEFADReader                  = (*qmSourceAdapter)(nil)
	_ qmiCoreReadyWaiter             = (*qmSourceAdapter)(nil)
	_ qmiCoreRecoveryRequester       = (*qmSourceAdapter)(nil)
)

// GetIMSIStrictLive 供 QMIBackend.GetIMSILive（函数内 strictLive 匿名接口）使用。
