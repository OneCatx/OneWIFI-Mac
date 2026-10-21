#![forbid(unsafe_code)]

use serde::{Deserialize, Serialize};
use serde_json::Value;

#[derive(Debug, Serialize)]
pub struct HealthResponse<'a> {
    pub ok: bool,
    pub port: &'a str,
    pub esim_available: bool,
    pub usb_device: Option<&'a str>,
    pub discovery_error: &'a str,
}

#[derive(Debug, Serialize)]
pub struct RuntimeResponse<'a> {
    pub ok: bool,
    pub backend: &'a str,
    pub version: &'a str,
    pub migration_stage: &'a str,
    pub hardware_ownership: bool,
    pub hardware_owner: &'a str,
    pub go_backend_connected: bool,
    pub go_backend_restarting: bool,
    pub go_backend_phase: &'a str,
    pub go_backend_generation: u64,
    pub go_restart_attempts: u32,
    pub proxy_contract: &'a str,
    pub sim_protocol_contract: &'a str,
    pub ike_wire_contract: &'a str,
    pub ims_protocol_contract: &'a str,
    pub sms_protocol_contract: &'a str,
    pub call_protocol_contract: &'a str,
    pub store_contract: &'a str,
    pub snapshot_schema_version: u16,
    pub ownership_schema_version: u16,
    pub default_runtime: &'a str,
    pub cutover_ready: bool,
}

#[derive(Clone, Copy, Debug, Serialize)]
pub struct MigrationComponent<'a> {
    pub id: &'a str,
    pub owner: &'a str,
    pub state: &'a str,
    pub requires_go_runtime: bool,
    pub blocks_go_removal: bool,
    pub detail: &'a str,
}

#[derive(Debug, Serialize)]
pub struct MigrationReadinessResponse<'a> {
    pub schema_version: u16,
    pub ready_to_remove_go: bool,
    /// Number of components that no longer require the Go runtime.  These
    /// counters make migration progress observable without inferring it from
    /// the component list in each client.
    pub rust_owned_components: usize,
    pub go_blocking_components: usize,
    pub rust_ownership_percent: u8,
    pub components: &'a [MigrationComponent<'a>],
}

#[derive(Debug, Serialize)]
pub struct ErrorResponse<'a> {
    pub error: &'a str,
    pub code: &'a str,
    pub retryable: bool,
}

#[derive(Clone, Debug, Deserialize, Serialize)]
pub struct UpstreamHealth {
    pub ok: bool,
    pub port: Option<String>,
    pub esim_available: Option<bool>,
    pub discovery_error: Option<String>,
    pub usb_device: Option<UpstreamUsbDevice>,
}

#[derive(Clone, Debug, Deserialize, Serialize)]
pub struct UpstreamUsbDevice {
    pub product: Option<String>,
    pub vendor: Option<String>,
    pub vendor_id: Option<String>,
    pub product_id: Option<String>,
    pub location_id: Option<String>,
    pub speed: Option<String>,
    pub mode: Option<String>,
    pub interfaces: Option<Vec<UpstreamUsbInterface>>,
}

#[derive(Clone, Debug, Deserialize, Serialize)]
pub struct UpstreamUsbInterface {
    pub number: Option<i64>,
    #[serde(rename = "class")]
    pub class_id: Option<i64>,
    pub subclass: Option<i64>,
    #[serde(rename = "protocol")]
    pub protocol_id: Option<i64>,
    pub endpoints: Option<i64>,
}

#[derive(Clone, Debug, Deserialize, Serialize)]
pub struct UpstreamDeviceStatus {
    pub imei: Option<String>,
    pub firmware: Option<String>,
    pub iccid: Option<String>,
    pub imsi: Option<String>,
    pub phone_number: Option<String>,
    pub native_spn: Option<String>,
    pub native_mcc: Option<String>,
    pub native_mnc: Option<String>,
    pub gid1: Option<String>,
    pub gid2: Option<String>,
    pub pnn: Option<Value>,
    pub opl: Option<Value>,
    pub sim_service_table: Option<Value>,
    #[serde(rename = "operator")]
    pub operator_name: Option<String>,
    pub sim_inserted: Option<bool>,
    pub signal_dbm: Option<i64>,
    pub signal_rsrp: Option<i64>,
    pub signal_rsrq: Option<i64>,
    pub signal_sinr: Option<i64>,
    pub nr5g_signal_sinr: Option<i64>,
    pub radio_band: Option<String>,
    pub radio_channel: Option<u64>,
    pub reg_status: Option<i64>,
    pub reg_status_text: Option<String>,
    pub ps_attached: Option<bool>,
    pub lac: Option<String>,
    pub cell_id: Option<String>,
    pub apn: Option<String>,
    pub ims_status: Option<i64>,
    pub network_mode: Option<String>,
    pub network_duplex: Option<String>,
    pub usbnet_mode: Option<i64>,
    pub operating_mode: Option<i64>,
    pub hardware_status: Option<String>,
    pub discovery_error: Option<String>,
    pub usb_device: Option<UpstreamUsbDevice>,
}

#[derive(Clone, Debug, Deserialize, Serialize)]
pub struct UpstreamReaderList {
    #[serde(default)]
    pub readers: Vec<String>,
    pub selected: Option<String>,
}

#[derive(Clone, Debug, Deserialize, Serialize)]
pub struct UpstreamReaderInfo {
    pub status: UpstreamReaderStatus,
    pub usim_info: Option<UpstreamUsimInfo>,
    pub epdg_fqdn: Option<String>,
}

#[derive(Clone, Debug, Deserialize, Serialize)]
pub struct UpstreamReaderStatus {
    pub connected: bool,
    pub reader_name: Option<String>,
    pub reader_generation: Option<u64>,
    pub has_card: bool,
    pub card_type: Option<String>,
    pub has_usim_info: bool,
    pub has_esim: bool,
}

#[derive(Clone, Debug, Deserialize, Serialize)]
pub struct UpstreamUsimInfo {
    pub iccid: String,
    pub imsi: String,
    pub phone_number: Option<String>,
    pub operator_name: Option<String>,
    pub operator_source: Option<String>,
    pub mcc: String,
    pub mnc: String,
    pub gid1: Option<String>,
    pub smsc: Option<String>,
    pub private_identity: Option<String>,
    pub public_identity: Option<String>,
    pub home_domain: Option<String>,
    pub loaded_at: Option<String>,
}

#[derive(Clone, Debug, Serialize)]
pub struct DeviceSnapshot {
    pub schema_version: u16,
    pub instance_id: String,
    pub revision: u64,
    pub ok: bool,
    pub complete: bool,
    pub captured_at_ms: u64,
    pub source: &'static str,
    pub module: ModuleSnapshot,
    pub reader: ReaderSnapshot,
    pub issues: Vec<SnapshotIssue>,
}

#[derive(Clone, Debug, Default, Serialize)]
pub struct ModuleSnapshot {
    pub health: Option<UpstreamHealth>,
    pub status: Option<UpstreamDeviceStatus>,
}

#[derive(Clone, Debug, Default, Serialize)]
pub struct ReaderSnapshot {
    #[serde(default)]
    pub readers: Vec<String>,
    pub selected: Option<String>,
    pub info: Option<UpstreamReaderInfo>,
    pub vowifi: Option<UpstreamReaderVoWiFiStatus>,
    pub inventory_source: String,
    pub inventory_revision: u64,
    #[serde(default)]
    pub devices: Vec<ReaderDiscoveryDevice>,
}

#[derive(Clone, Debug, Serialize)]
pub struct ReaderDiscoveryDevice {
    pub name: String,
    pub card_state: &'static str,
    pub in_use: bool,
    pub muted: bool,
    pub atr_hex: String,
}

#[derive(Clone, Debug, Deserialize, Serialize)]
pub struct UpstreamReaderVoWiFiStatus {
    pub reader_id: Option<String>,
    pub sim_id: Option<String>,
    pub reader_generation: Option<u64>,
    pub available: bool,
    pub mcc: Option<String>,
    pub mnc: Option<String>,
    pub imsi: Option<String>,
    pub imei: Option<String>,
    pub epdg_fqdn: Option<String>,
    pub epdg_source: Option<String>,
    pub profile_id: Option<String>,
    pub profile_name: Option<String>,
    pub profile_source: Option<String>,
    pub tunnel_state: String,
    pub ims_state: String,
    pub message: Option<String>,
    pub local_ipv4: Option<String>,
    pub local_ipv6: Option<String>,
    pub pcscf: Option<Vec<String>>,
    pub ike_encryption: Option<String>,
    pub esp_encryption: Option<String>,
    pub registered_msisdn: Option<String>,
    pub ims_transport: Option<String>,
    pub ims_security_mode: Option<String>,
    pub sms_ready: Option<bool>,
    pub sms_state: Option<String>,
    pub call_ready: Option<bool>,
    pub call_state: Option<String>,
    #[serde(default)]
    pub progress_step: u8,
    pub progress_desc: Option<String>,
    #[serde(flatten)]
    pub extra: serde_json::Map<String, Value>,
}

#[derive(Clone, Debug, Serialize)]
pub struct SnapshotIssue {
    pub endpoint: &'static str,
    pub code: &'static str,
    pub message: String,
    pub retryable: bool,
}

#[cfg(test)]
mod tests {
    use super::{
        UpstreamDeviceStatus, UpstreamHealth, UpstreamReaderInfo, UpstreamReaderList,
        UpstreamReaderVoWiFiStatus,
    };

    const HEALTH_CONNECTED: &str = include_str!("../fixtures/health-connected.json");
    const STATUS_CONNECTED: &str = include_str!("../fixtures/status-connected.json");
    const READER_LIST_CONNECTED: &str = include_str!("../fixtures/reader-list-connected.json");
    const READER_INFO_CONNECTED: &str = include_str!("../fixtures/reader-info-connected.json");
    const READER_VOWIFI_CONNECTED: &str = include_str!("../fixtures/reader-vowifi-connected.json");

    #[test]
    fn decodes_disconnected_module_shape() {
        let status: UpstreamDeviceStatus = serde_json::from_str(
            r#"{"operator":"未连接","signal_dbm":null,"sim_inserted":false,"usb_device":null}"#,
        )
        .expect("status fixture");
        assert_eq!(status.operator_name.as_deref(), Some("未连接"));
        assert_eq!(status.signal_dbm, None);
        assert_eq!(status.sim_inserted, Some(false));
    }

    #[test]
    fn preserves_module_metrics_and_typed_usb_shape() {
        let status: UpstreamDeviceStatus = serde_json::from_str(
            r#"{
                "operator":"Example",
                "signal_dbm":-83,
                "signal_rsrp":-102,
                "signal_rsrq":-12,
                "signal_sinr":18,
                "reg_status":1,
                "ps_attached":true,
                "ims_status":1,
                "usbnet_mode":1,
                "usb_device":{
                    "product":"Modem",
                    "vendor_id":"2ca3",
                    "interfaces":[{"number":2,"class":255,"protocol":1}]
                }
            }"#,
        )
        .expect("full module fixture");
        assert_eq!(status.signal_rsrp, Some(-102));
        assert_eq!(status.signal_sinr, Some(18));
        assert_eq!(status.usbnet_mode, Some(1));
        let usb = status.usb_device.expect("typed USB device");
        assert_eq!(usb.vendor_id.as_deref(), Some("2ca3"));
        assert_eq!(usb.interfaces.expect("interfaces")[0].class_id, Some(255));
    }

    #[test]
    fn replays_sanitized_connected_device_contract() {
        let health: UpstreamHealth =
            serde_json::from_str(HEALTH_CONNECTED).expect("connected health fixture");
        assert!(health.ok);
        let health_usb = health.usb_device.expect("fixture USB device");
        assert_eq!(health_usb.product_id.as_deref(), Some("4006"));
        assert_eq!(
            health_usb.interfaces.expect("fixture interfaces")[0].protocol_id,
            Some(1)
        );

        let status: UpstreamDeviceStatus =
            serde_json::from_str(STATUS_CONNECTED).expect("connected status fixture");
        assert_eq!(status.operator_name.as_deref(), Some("Fixture Carrier"));
        assert_eq!(status.signal_rsrp, Some(-102));
        assert_eq!(status.radio_channel, Some(1300));
        assert_eq!(status.ims_status, Some(1));
        assert_eq!(status.operating_mode, Some(0));
        let replayed = serde_json::to_value(&status).expect("serialize connected status");
        assert_eq!(replayed["phone_number"], "+10000000000");
        assert_eq!(replayed["network_duplex"], "FDD");
        assert_eq!(replayed["sim_service_table"]["kind"], "ust");

        let list: UpstreamReaderList =
            serde_json::from_str(READER_LIST_CONNECTED).expect("connected reader list fixture");
        let info: UpstreamReaderInfo =
            serde_json::from_str(READER_INFO_CONNECTED).expect("connected reader info fixture");
        let vowifi: UpstreamReaderVoWiFiStatus =
            serde_json::from_str(READER_VOWIFI_CONNECTED).expect("connected reader VoWiFi fixture");
        assert_eq!(list.selected.as_deref(), info.status.reader_name.as_deref());
        assert!(info.status.connected);
        assert_eq!(
            info.usim_info
                .expect("fixture USIM identity")
                .operator_name
                .as_deref(),
            Some("Fixture Carrier")
        );
        assert_eq!(vowifi.tunnel_state, "connected");
        assert_eq!(vowifi.ims_state, "registered");
        assert_eq!(vowifi.sms_ready, Some(true));
        assert_eq!(vowifi.call_ready, Some(true));
        let replayed = serde_json::to_value(vowifi).expect("serialize VoWiFi fixture");
        assert_eq!(replayed["transport_mode"], "enhanced-mode-utun");
    }

    #[test]
    fn reader_list_allows_no_selection() {
        let list: UpstreamReaderList =
            serde_json::from_str(r#"{"readers":["Reader A"]}"#).expect("reader list");
        assert_eq!(list.readers, ["Reader A"]);
        assert_eq!(list.selected, None);
    }

    #[test]
    fn reader_info_accepts_optional_identity_fields() {
        let info: UpstreamReaderInfo = serde_json::from_str(
            r#"{"status":{"connected":true,"reader_name":"Reader A","has_card":true,"card_type":"euicc","has_usim_info":false,"has_esim":true}}"#,
        )
        .expect("reader info");
        assert!(info.status.connected);
        assert_eq!(info.status.reader_name.as_deref(), Some("Reader A"));
        assert!(info.usim_info.is_none());
    }
}
