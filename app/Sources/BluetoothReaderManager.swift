import Combine
import CoreBluetooth
import Foundation

enum BluetoothReaderErrorCode: String {
    case unauthorized = "bluetooth_unauthorized"
    case poweredOff = "bluetooth_powered_off"
    case unsupported = "bluetooth_unsupported"
    case initializationTimeout = "bluetooth_initialization_timeout"
    case connectFailed = "bluetooth_connect_failed"
    case connectionTimeout = "bluetooth_connection_timeout"
    case serviceDiscoveryTimeout = "bluetooth_service_discovery_timeout"
    case characteristicDiscoveryTimeout = "bluetooth_characteristic_discovery_timeout"
    case notificationTimeout = "bluetooth_notification_timeout"
    case writeFailed = "bluetooth_write_failed"
    case serviceMissing = "estk_service_missing"
    case characteristicMissing = "estk_characteristic_missing"
    case notificationsFailed = "estk_notifications_failed"
    case disconnected = "bluetooth_disconnected"
    case apduBusy = "estk_apdu_busy"
    case apduTimeout = "estk_apdu_timeout"
    case apduMalformed = "estk_apdu_malformed"
}

enum BluetoothReaderState: Equatable {
    case idle, scanning, connecting, discovering, connected, reconnecting, disconnected, failed
}

/// Reader protocol types.
/// - legacy: ESTKme-RED firmware with 3-byte framing
/// - ccid: ESTKme-RED with CCID (10-byte header)
/// - beeSim: 9esim / BeeSIM cards with [total, index] framing
private enum ESTKProtocol { case legacy, ccid, beeSim }

/// Owns the CoreBluetooth central for the lifetime of the application. BLE
/// discovery is not a PC/SC session and is not advertised as an AKA-capable
/// reader until the APDU backend is connected separately.
final class BluetoothReaderManager: NSObject, ObservableObject, CBCentralManagerDelegate, CBPeripheralDelegate {
    static let shared = BluetoothReaderManager()

    @Published private(set) var state: BluetoothReaderState = .idle
    @Published private(set) var errorCode: BluetoothReaderErrorCode?
    @Published private(set) var detail = "正在检查蓝牙…"
    @Published private(set) var devices: [UUID: String] = [:]
    @Published private(set) var connectedID: UUID?
    // Discovery is intentionally independent from the GATT/APDU link.  A
    // user may look for another reader while the selected one is carrying a
    // live AKA/IMS session; treating that re-scan as a disconnect would tear
    // down the reader session below the UI.
    @Published private(set) var isScanning = false

    private var central: CBCentralManager?
    private let centralInitializationDeadline = BLEPhaseDeadline()
    private let centralInitializationTimeoutSeconds: TimeInterval = 10
    private var centralInitializationPending = false
    private var deferredCentralStateCallback = false
    private var scanRequested = false
    private var peripherals: [UUID: CBPeripheral] = [:]
    // The active APDU session keeps its own CBPeripheral object. Discovery
    // may return a newer object for the same UUID, but that object is adopted
    // only when a fresh connection attempt begins.
    private var latestDiscoveredPeripherals: [UUID: CBPeripheral] = [:]
    // A live advertisement is a fresher object source than CoreBluetooth's
    // retrieved-connected cache, including for readers not yet selected.
    private var advertisedPeripheralIDs = Set<UUID>()
    private struct StagedReader {
        let name: String
        let proto: ESTKProtocol
        let nameIsAdvertised: Bool
    }
    // Discovery may refine a reader while an APDU link is in flight. Its
    // identity and framing are adopted only at the next connection attempt.
    private var stagedReaders: [UUID: StagedReader] = [:]
    private var advertisedServices: [UUID: Set<CBUUID>] = [:]
    private var protocols: [UUID: ESTKProtocol] = [:]
    // A name read from advertisementData is newer than CoreBluetooth's
    // peripheral.name cache. A name obtained only from that cache is not:
    // later nameless advertisements may carry a corrected cached name.
    private var readersWithAdvertisedName = Set<UUID>()

    // ESTKme-RED service/characteristics
    private let estkServiceUUID = CBUUID(string: "4553")
    private let estkNotifyUUID = CBUUID(string: "544b")
    private let estkWriteUUID = CBUUID(string: "6d65")

    // BeeSIM (9esim) service/characteristics
    // Service: 0000ae30-0000-1000-8000-00805f9b34fb
    // TX (write): AE01, RX (notify): AE02
    private let beeSimServiceUUID = CBUUID(string: "AE30")
    private let beeSimWriteUUID = CBUUID(string: "AE01")
    private let beeSimNotifyUUID = CBUUID(string: "AE02")

    private var notifyCharacteristic: CBCharacteristic?
    private var writeCharacteristic: CBCharacteristic?
    private var notificationSetupStarted = false
    private var cardHandshakeStarted = false
    private var desiredID: UUID?
    private var manualDisconnect = false
    private var reconnectAttempt = 0
    private var reconnectWork: DispatchWorkItem?
    private let phaseDeadline = BLEPhaseDeadline()
    private let phaseTimeoutSeconds: TimeInterval = 30
    private let preferredReaderKey = "Proletarius.PreferredESTKReaderID"
    private var pendingReply: ((Result<Data, Error>) -> Void)?
    private var replyExpected: Int?
    private var replyBytes = Data()
    private var replyTimeout: DispatchWorkItem?
    private var writeQueue: [Data] = []
    private var writeIndex = 0
    private var writeType: CBCharacteristicWriteType?
    private var writeAwaitingResponse = false

    // BeeSIM response reassembly state
    private var beeSimRxBuffer = Data()

    private override init() { super.init() }

    // File-based BLE diagnostic log for debugging connection failures.
    private func bleDiagLog(_ msg: String) {
        let line = "\(ISO8601DateFormatter().string(from: Date())) \(msg)\n"
        let dir = SwitchRuntimePaths.dataDirectory()
        let path = dir.appendingPathComponent("ble_diag.log")
        try? FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        if FileManager.default.fileExists(atPath: path.path) {
            if let handle = try? FileHandle(forWritingTo: path) {
                handle.seekToEndOfFile()
                if let d = line.data(using: .utf8) { handle.write(d) }
                handle.closeFile()
            }
        } else {
            try? line.data(using: .utf8)?.write(to: path)
        }
    }

    // MARK: - Protocol detection

    private func detectProtocol(name: String) -> ESTKProtocol {
        let lower = name.lowercased()
        // ESTKme-RED uses CCID framing (10-byte header) when the name
        // contains "estkme" (covers "ESTKme-RED", "ESTKme_RED", "estkme red",
        // etc.). This handles both USB and BLE modes.
        if lower.contains("estkme") {
            return .ccid
        }
        // Only "beesim" (exact) maps to the BeeSIM [total,index] framing.
        if lower.contains("beesim") || lower.contains("bee_sim") {
            return .beeSim
        }
        // 9eSIM cards use BeeSIM protocol (AE30 service, [total,index] framing).
        if lower.contains("9esim") {
            return .beeSim
        }
        // All other devices default to legacy ESTKme protocol (3-byte framing).
        return .legacy
    }

    private func currentProtocol() -> ESTKProtocol? {
        let id = connectedID ?? desiredID
        guard let id else { return nil }
        return protocols[id]
    }

    // MARK: - Scan

    private func importSystemConnectedReaders(_ central: CBCentralManager) {
        // Retrieve peripherals for both ESTKme and BeeSIM service UUIDs.
        let allServiceUUIDs = [estkServiceUUID, beeSimServiceUUID]
        for peripheral in central.retrieveConnectedPeripherals(withServices: allServiceUUIDs) {
            let id = peripheral.identifier
            // CoreBluetooth's retrieved object/name can be older than a
            // same-UUID advertisement already staged for the next attempt.
            if advertisedPeripheralIDs.contains(id) { continue }
            latestDiscoveredPeripherals[id] = peripheral
            advertisedServices[id] = Set(allServiceUUIDs)
            // An in-flight or established APDU session owns its identity and
            // framing. A discovery-only re-scan must not replace it with a
            // nil/stale peripheral.name returned by CoreBluetooth.
            if id == connectedID || id == desiredID {
                let hasAdvertisedName = readersWithAdvertisedName.contains(id)
                // Retrieval refreshes the object reference, not an active
                // reader's protocol: peripheral.name is a stale cache.
                let name = devices[id] ?? peripheral.name ?? ""
                let displayName = name.isEmpty ? "eSIM Reader" : name
                stagedReaders[id] = StagedReader(name: displayName,
                                                  proto: protocols[id] ?? detectProtocol(name: displayName),
                                                  nameIsAdvertised: hasAdvertisedName)
                continue
            }
            peripherals[id] = peripheral
            // The same name rule applies to another discovered reader whose
            // advertised LocalName is newer than the retrieved cache.
            if readersWithAdvertisedName.contains(id) { continue }
            let name = peripheral.name ?? ""
            devices[id] = name.isEmpty ? "eSIM Reader" : name
            protocols[id] = detectProtocol(name: name)
            ReaderOperationLogger.write(module: "bluetooth", action: "retrieve_connected",
                                        state: "found", result: "ok")
        }
    }

    private var hasActiveLink: Bool {
        switch state {
        case .connecting, .discovering, .connected, .reconnecting:
            return desiredID != nil || connectedID != nil
        case .idle, .scanning, .disconnected, .failed:
            return false
        }
    }

    private func clearInactiveLinkState(on central: CBCentralManager) {
        // A failed or abandoned attempt may still have a CoreBluetooth
        // connection in flight.  Clear its identity before cancellation so a
        // delayed callback cannot revive it or schedule an unwanted reconnect.
        let staleIDs = Set([desiredID, connectedID].compactMap { $0 })
        phaseDeadline.cancel()
        desiredID = nil
        connectedID = nil
        notifyCharacteristic = nil
        writeCharacteristic = nil
        reconnectWork?.cancel()
        reconnectWork = nil
        for id in staleIDs {
            if let peripheral = peripherals[id] {
                central.cancelPeripheralConnection(peripheral)
            }
        }
    }

    private func clearDiscoveryCache() {
        devices.removeAll()
        peripherals.removeAll()
        latestDiscoveredPeripherals.removeAll()
        advertisedPeripheralIDs.removeAll()
        stagedReaders.removeAll()
        advertisedServices.removeAll()
        protocols.removeAll()
        readersWithAdvertisedName.removeAll()
    }

    private func beginDiscovery(on central: CBCentralManager) {
        importSystemConnectedReaders(central)
        central.scanForPeripherals(withServices: nil,
            options: [CBCentralManagerScanOptionAllowDuplicatesKey: true])
        isScanning = true
    }

    private func failBluetoothAuthorization() {
        centralInitializationPending = false
        deferredCentralStateCallback = false
        centralInitializationDeadline.cancel()
        reconnectWork?.cancel()
        reconnectWork = nil
        central?.stopScan()
        finishReply(.failure(BluetoothReaderTransportError(code: .unauthorized)))
        connectedID = nil
        notifyCharacteristic = nil
        writeCharacteristic = nil
        fail(.unauthorized, "请在系统设置中允许 OneWIFI 使用蓝牙")
    }

    private func awaitCentralInitialization() {
        guard centralInitializationPending else { return }
        if CBManager.authorization == .notDetermined {
            detail = "请在系统提示中允许 OneWIFI 使用蓝牙，随后将自动继续扫描…"
            centralInitializationDeadline.arm(after: 1) { [weak self] in
                self?.awaitCentralInitialization()
            }
        } else if CBManager.authorization != .allowedAlways {
            failBluetoothAuthorization()
        } else {
            detail = "正在检查蓝牙…"
            if deferredCentralStateCallback, let central {
                deferredCentralStateCallback = false
                centralManagerDidUpdateState(central)
                return
            }
            centralInitializationDeadline.arm(after: centralInitializationTimeoutSeconds) { [weak self] in
                guard let self, self.centralInitializationPending else { return }
                self.centralInitializationPending = false
                self.deferredCentralStateCallback = false
                self.central = nil
                self.fail(.initializationTimeout, "蓝牙初始化超时，请确认系统蓝牙和应用权限后重新扫描")
            }
        }
    }

    func scan() {
        scanRequested = true
        if CBManager.authorization != .notDetermined && CBManager.authorization != .allowedAlways {
            failBluetoothAuthorization()
        } else if central == nil {
            errorCode = nil
            state = .idle
            detail = "正在检查蓝牙…"
            centralInitializationPending = true
            deferredCentralStateCallback = false
            awaitCentralInitialization()
            central = CBCentralManager(delegate: self, queue: .main)
        } else if CBManager.authorization == .notDetermined {
            errorCode = nil
            state = .idle
            centralInitializationPending = true
            awaitCentralInitialization()
        } else if let central, central.state == .poweredOn {
            centralInitializationPending = false
            deferredCentralStateCallback = false
            centralInitializationDeadline.cancel()
            if hasActiveLink {
                // Re-scan is discovery-only while a reader is connected,
                // connecting, or automatically reconnecting.  Never clear
                // desiredID/connectedID, characteristics, or reconnectWork
                // here: all of them belong to the active card session.
                beginDiscovery(on: central)
            } else {
                // A terminal failed/disconnected attempt can be discarded so
                // the next connection starts from a clean discovery cache.
                clearInactiveLinkState(on: central)
                clearDiscoveryCache()
                errorCode = nil
                state = .scanning
                detail = "正在扫描蓝牙读卡器…"
                beginDiscovery(on: central)
            }
        } else if let central {
            errorCode = nil
            state = .idle
            detail = "正在检查蓝牙…"
            centralInitializationPending = true
            awaitCentralInitialization()
            switch central.state {
            case .unauthorized, .poweredOff, .unsupported:
                centralManagerDidUpdateState(central)
            default: break
            }
        }
        ReaderOperationLogger.write(module: "bluetooth", action: "scan", state: "requested", result: "pending")
    }

    func stopScan() {
        scanRequested = false
        central?.stopScan()
        isScanning = false
        let wasInitializing = centralInitializationPending
        centralInitializationPending = false
        centralInitializationDeadline.cancel()
        // A consent sheet may outlive this UI sheet. Keep its central so a
        // later grant is observed and the next explicit scan can reuse it.
        if wasInitializing && CBManager.authorization != .notDetermined {
            central = nil
            deferredCentralStateCallback = false
        }
        if wasInitializing || state == .scanning {
            state = .idle
            detail = "蓝牙扫描已停止"
        }
    }

    // MARK: - Connect / Disconnect

    func connect(_ id: UUID) {
        if connectedID == id { return }
        guard CBManager.authorization == .allowedAlways else {
            if CBManager.authorization != .notDetermined { failBluetoothAuthorization() }
            return
        }
        // Clear stale state from a session that the Go backend may have
        // already torn down via ReconcilePresence during the GATT handshake.
        if let oldID = desiredID, oldID != id, let pending = peripherals[oldID] {
            central?.cancelPeripheralConnection(pending)
        }
        if let oldID = connectedID, oldID != id, let pending = peripherals[oldID] {
            central?.cancelPeripheralConnection(pending)
        }
        phaseDeadline.cancel()
        desiredID = nil
        connectedID = nil
        notifyCharacteristic = nil
        writeCharacteristic = nil
        guard let peripheral = peripherals[id], central?.state == .poweredOn else {
            bleDiagLog("connect: REJECTED peripheral=\(peripherals[id] != nil) poweredOn=\(central?.state == .poweredOn)")
            fail(.connectFailed, "蓝牙设备不可用，请重新扫描")
            return
        }
        bleDiagLog("connect: name=\(devices[id] ?? "?") proto=\(protocols[id] ?? .legacy)")
        reconnectWork?.cancel()
        reconnectAttempt = 0
        manualDisconnect = false
        desiredID = id
        stopScan()
        peripheral.delegate = self
        startConnectionAttempt(id, detail: "正在连接 \(devices[id] ?? "蓝牙读卡器")…")
        ReaderOperationLogger.write(module: "bluetooth", action: "connect", state: "connecting", result: "pending")
    }

    func disconnect() {
        manualDisconnect = true
        if centralInitializationPending { central = nil }
        centralInitializationPending = false
        deferredCentralStateCallback = false
        centralInitializationDeadline.cancel()
        UserDefaults.standard.removeObject(forKey: preferredReaderKey)
        let pendingID = connectedID ?? desiredID
        phaseDeadline.cancel()
        desiredID = nil
        connectedID = nil
        notifyCharacteristic = nil
        writeCharacteristic = nil
        reconnectWork?.cancel()
        reconnectWork = nil
        finishReply(.failure(BluetoothReaderTransportError(code: .disconnected)))
        // A cancelled connection may never produce didDisconnect. Publish the
        // terminal state here, after failing any pending handshake callback.
        state = .disconnected
        detail = "蓝牙已断开"
        errorCode = nil
        if let id = pendingID, let peripheral = peripherals[id] {
            central?.cancelPeripheralConnection(peripheral)
        }
        ReaderOperationLogger.write(module: "bluetooth", action: "disconnect", state: "disconnected", result: "ok")
    }

    private func fail(_ code: BluetoothReaderErrorCode, _ message: String) {
        phaseDeadline.cancel()
        isScanning = false
        errorCode = code
        state = .failed
        detail = message
        ReaderOperationLogger.write(module: "bluetooth", action: "state", state: "failed", result: "error", errorCode: code.rawValue)
    }

    private func armPhaseDeadline(
        for id: UUID,
        expectedState: BluetoothReaderState,
        code: BluetoothReaderErrorCode,
        message: String
    ) {
        phaseDeadline.arm(after: phaseTimeoutSeconds) { [weak self] in
            guard let self, self.desiredID == id, self.state == expectedState else { return }
            self.fail(code, message)
            if let peripheral = self.peripherals[id] {
                self.central?.cancelPeripheralConnection(peripheral)
            }
            self.scheduleReconnect()
        }
    }

    private func startConnectionAttempt(_ id: UUID, detail: String) {
        guard CBManager.authorization == .allowedAlways else {
            if CBManager.authorization != .notDetermined { failBluetoothAuthorization() }
            return
        }
        guard desiredID == id, let central, central.state == .poweredOn else { return }
        if let latest = latestDiscoveredPeripherals[id] {
            peripherals[id] = latest
        }
        guard let peripheral = peripherals[id] else { return }
        // The advertised object is now the selected attempt; a later system
        // retrieval may again become a candidate for a *future* attempt.
        advertisedPeripheralIDs.remove(id)
        if let staged = stagedReaders.removeValue(forKey: id) {
            devices[id] = staged.name
            protocols[id] = staged.proto
            if staged.nameIsAdvertised { readersWithAdvertisedName.insert(id) }
            else { readersWithAdvertisedName.remove(id) }
        }
        reconnectWork?.cancel()
        reconnectWork = nil
        phaseDeadline.cancel()
        notifyCharacteristic = nil
        writeCharacteristic = nil
        notificationSetupStarted = false
        cardHandshakeStarted = false
        state = .connecting
        self.detail = detail
        peripheral.delegate = self
        armPhaseDeadline(
            for: id,
            expectedState: .connecting,
            code: .connectionTimeout,
            message: "蓝牙连接等待设备响应超时，稍后将自动重试")
        central.connect(peripheral)
    }

    // MARK: - APDU Exchange

    /// The only raw exchange entry point. Callers must be serialized by
    /// the IPC bridge; an in-flight request is never overwritten.
    func exchangeRaw(_ frame: Data, completion: @escaping (Result<Data, Error>) -> Void) {
        guard pendingReply == nil else {
            completion(.failure(BluetoothReaderTransportError(code: .apduBusy)))
            return
        }
        guard let id = connectedID ?? desiredID, let peripheral = peripherals[id],
              peripheral.state == .connected, writeCharacteristic != nil,
              notifyCharacteristic?.isNotifying == true else {
            completion(.failure(BluetoothReaderTransportError(code: .disconnected)))
            return
        }
        pendingReply = completion
        replyExpected = nil
        replyBytes = Data()
        beeSimRxBuffer = Data()
        // Profile download involves long BPP transfers; 120s avoids premature
        // timeout during large profile writes over BLE.
        let timeoutSeconds: Double = 120
        let timeout = DispatchWorkItem { [weak self] in
            self?.invalidateTransport(.apduTimeout)
        }
        replyTimeout = timeout
        DispatchQueue.main.asyncAfter(deadline: .now() + timeoutSeconds, execute: timeout)

        let proto = protocols[id] ?? .legacy
        let dataToSend: Data
        switch proto {
        case .beeSim:
            guard let encoded = encodeBeeSimFrames(frame, peripheral: peripheral) else {
                // BeeSIM carries frame counts in one byte. Reject oversized
                // requests before writing, without tearing down a healthy link.
                finishReply(.failure(BluetoothReaderTransportError(code: .apduMalformed)))
                return
            }
            dataToSend = encoded
        case .ccid, .legacy:
            dataToSend = frame
        }

        // Match ESTK/NekokoLPA2's transport choice: use Write Without Response
        // only when the characteristic does not also offer Write. A writer
        // that supports only Write must be paced by didWriteValue callbacks.
        let supportsWrite = writeCharacteristic!.properties.contains(.write)
        let supportsWriteWithoutResponse = writeCharacteristic!.properties.contains(.writeWithoutResponse)
        guard supportsWrite || supportsWriteWithoutResponse else {
            finishReply(.failure(BluetoothReaderTransportError(code: .characteristicMissing)))
            return
        }
        writeType = (!supportsWrite && supportsWriteWithoutResponse) ? .withoutResponse : .withResponse
        writeAwaitingResponse = false
        let chunkSize = max(1, peripheral.maximumWriteValueLength(for: writeType!))
        writeQueue = stride(from: 0, to: dataToSend.count, by: chunkSize).map { offset in
            dataToSend.subdata(in: offset..<min(offset + chunkSize, dataToSend.count))
        }
        writeIndex = 0
        flushWriteQueue(peripheral)
    }

    func exchangeAPDU(_ apdu: Data, completion: @escaping (Result<Data, Error>) -> Void) {
        guard apdu.count <= 65535 else {
            completion(.failure(BluetoothReaderTransportError(code: .apduMalformed)))
            return
        }
        let id = connectedID ?? desiredID
        guard let id else {
            completion(.failure(BluetoothReaderTransportError(code: .disconnected)))
            return
        }
        let proto = protocols[id] ?? .legacy

        switch proto {
        case .beeSim:
            // BeeSIM: raw APDU goes through the [total, index] framing
            // in exchangeRaw, no additional header needed here.
            exchangeRaw(apdu) { result in
                switch result {
                case .failure(let error): completion(.failure(error))
                case .success(let response):
                    // BeeSIM response is the reassembled payload (no CCID header)
                    completion(.success(response))
                }
            }
        case .ccid:
            var frame = Data([0x6f, UInt8(apdu.count & 0xff), UInt8((apdu.count >> 8) & 0xff),
                              UInt8((apdu.count >> 16) & 0xff), UInt8((apdu.count >> 24) & 0xff),
                              0, 0, 0, 0, 0])
            frame.append(apdu)
            exchangeRaw(frame) { result in
                switch result {
                case .failure(let error): completion(.failure(error))
                case .success(let response):
                    guard response.count >= 10 else { completion(.failure(BluetoothReaderTransportError(code: .apduMalformed))); return }
                    let length = Int(response[1]) | (Int(response[2]) << 8) | (Int(response[3]) << 16) | (Int(response[4]) << 24)
                    guard response.count >= 10 + length else { completion(.failure(BluetoothReaderTransportError(code: .apduMalformed))); return }
                    completion(.success(response.subdata(in: 10..<(10 + length))))
                }
            }
        case .legacy:
            var frame = Data([4, UInt8(apdu.count & 0xff), UInt8((apdu.count >> 8) & 0xff)])
            frame.append(apdu)
            exchangeRaw(frame, completion: completion)
        }
    }

    func powerCycle(completion: @escaping (Result<Data, Error>) -> Void) {
        let proto = currentProtocol() ?? .legacy
        switch proto {
        case .beeSim:
            // BeeSIM reset: A0 3F 00 00 00
            exchangeRaw(Data([0xA0, 0x3F, 0x00, 0x00, 0x00]), completion: completion)

        case .ccid:
            let powerOff = Data([0x63, 0, 0, 0, 0, 0, 0, 0, 0, 0])
            let powerOn = Data([0x62, 1, 0, 0, 0, 0, 0, 0, 0, 0, 1])
            exchangeRaw(powerOff) { [weak self] result in
                switch result {
                case .failure(let error): completion(.failure(error))
                case .success:
                    self?.exchangeRaw(powerOn, completion: completion)
                }
            }
        case .legacy:
            let powerOff = Data([3, 0, 0])
            let powerOn = Data([3, 2, 0, 1, 1])
            exchangeRaw(powerOff) { [weak self] result in
                switch result {
                case .failure(let error): completion(.failure(error))
                case .success:
                    self?.exchangeRaw(powerOn, completion: completion)
                }
            }
        }
    }

    // MARK: - BeeSIM Frame Encoding

    /// Encodes raw APDU data into BeeSIM [total, index, ...payload] frames.
    /// Each frame's payload is at most (MTU - 3 - 2) bytes to fit in one GATT write.
    private func encodeBeeSimFrames(_ data: Data, peripheral: CBPeripheral) -> Data? {
        let mtu = peripheral.maximumWriteValueLength(for: .withoutResponse)
        let framePayload = max(18, min(244, mtu) - 2)
        let total = data.isEmpty ? 1 : (data.count + framePayload - 1) / framePayload
        guard total <= Int(UInt8.max) else { return nil }
        var result = Data()
        for i in 0..<total {
            let start = i * framePayload
            let end = min(start + framePayload, data.count)
            result.append(UInt8(total))
            result.append(UInt8(i + 1))
            result.append(data.subdata(in: start..<end))
        }
        return result
    }

    // MARK: - Write Queue

    private func flushWriteQueue(_ peripheral: CBPeripheral) {
        guard let writeCharacteristic, let writeType else { return }
        switch writeType {
        case .withResponse:
            guard !writeAwaitingResponse, writeIndex < writeQueue.count else { return }
            let chunk = writeQueue[writeIndex]
            writeIndex += 1
            writeAwaitingResponse = true
            peripheral.writeValue(chunk, for: writeCharacteristic, type: .withResponse)
        case .withoutResponse:
            while peripheral.canSendWriteWithoutResponse && writeIndex < writeQueue.count {
                peripheral.writeValue(writeQueue[writeIndex], for: writeCharacteristic, type: .withoutResponse)
                writeIndex += 1
            }
            if writeIndex == writeQueue.count {
                writeQueue.removeAll(keepingCapacity: true)
                writeIndex = 0
                self.writeType = nil
            }
        @unknown default:
            invalidateTransport(.writeFailed)
        }
    }

    func peripheralIsReady(toSendWriteWithoutResponse peripheral: CBPeripheral) {
        guard peripherals[peripheral.identifier] === peripheral else { return }
        flushWriteQueue(peripheral)
    }

    func peripheral(_ peripheral: CBPeripheral, didWriteValueFor characteristic: CBCharacteristic, error: Error?) {
        guard peripherals[peripheral.identifier] === peripheral,
              (connectedID == peripheral.identifier || desiredID == peripheral.identifier),
              let selectedWriter = writeCharacteristic,
              characteristic === selectedWriter,
              writeType == .withResponse,
              writeAwaitingResponse,
              pendingReply != nil else { return }
        if error != nil {
            invalidateTransport(.writeFailed)
            return
        }
        writeAwaitingResponse = false
        if writeIndex >= writeQueue.count {
            writeQueue.removeAll(keepingCapacity: true)
            writeIndex = 0
            writeType = nil
            return
        }
        flushWriteQueue(peripheral)
    }

    // MARK: - Response Handling

    private func finishReply(_ result: Result<Data, Error>) {
        guard let callback = pendingReply else { return }
        pendingReply = nil
        replyTimeout?.cancel()
        replyTimeout = nil
        replyExpected = nil
        replyBytes.removeAll()
        beeSimRxBuffer.removeAll()
        writeQueue.removeAll()
        writeIndex = 0
        writeType = nil
        writeAwaitingResponse = false
        callback(result)
    }

    private func invalidateTransport(_ code: BluetoothReaderErrorCode) {
        let id = connectedID ?? desiredID
        connectedID = nil
        notifyCharacteristic = nil
        writeCharacteristic = nil
        // Revoke the transport before invoking a handshake continuation. A
        // failed optional claim must not enqueue another APDU on this link.
        finishReply(.failure(BluetoothReaderTransportError(code: code)))
        errorCode = code
        state = .reconnecting
        detail = code == .notificationsFailed
            ? "蓝牙通知通道中断，正在恢复连接…"
            : "蓝牙 APDU 通道异常，正在恢复连接…"
        if let id, let peripheral = peripherals[id] {
            central?.cancelPeripheralConnection(peripheral)
        }
        ReaderOperationLogger.write(module: "bluetooth", action: "exchange", state: "invalidated", result: "error", errorCode: code.rawValue)
        // Do not depend solely on CoreBluetooth delivering didDisconnect: a
        // radio reset can suppress/late-deliver that callback. Scheduling is
        // A later didDisconnect preserves the already scheduled retry.
        scheduleReconnect()
    }

    func peripheral(_ peripheral: CBPeripheral, didUpdateValueFor characteristic: CBCharacteristic, error: Error?) {
        // A connection owns one selected notification characteristic. Never
        // accept a value from another service while an APDU is in flight: a
        // peripheral may expose unrelated notify characteristics, and their
        // bytes must not be interpreted as an ESTK/CCID response.
        guard peripherals[peripheral.identifier] === peripheral,
              (connectedID == peripheral.identifier || desiredID == peripheral.identifier),
              let selectedNotify = notifyCharacteristic,
              characteristic === selectedNotify,
              pendingReply != nil else { return }
        if let error { finishReply(.failure(error)); return }
        guard let value = characteristic.value else { return }

        let proto = currentProtocol() ?? .legacy

        switch proto {
        case .beeSim:
            handleBeeSimNotification(value)
        case .ccid:
            handleCcidNotification(value)
        case .legacy:
            handleLegacyNotification(value)
        }
    }

    // MARK: - BeeSIM Response Reassembly

    private func handleBeeSimNotification(_ data: Data) {
        guard data.count >= 2 else { return }
        let total = Int(data[0])
        let index = Int(data[1])
        if index == 1 {
            beeSimRxBuffer.removeAll()
        }
        if data.count > 2 {
            beeSimRxBuffer.append(data.subdata(in: 2..<data.count))
        }
        if index == total {
            // Complete response received
            finishReply(.success(beeSimRxBuffer))
        }
    }

    // MARK: - CCID Response Reassembly

    private func handleCcidNotification(_ data: Data) {
        replyBytes.append(data)
        if replyExpected == nil {
            let headerLength = 10
            guard replyBytes.count >= headerLength else { return }
            let bytes = [UInt8](replyBytes)
            if bytes[7] & 0x80 != 0 && bytes[1...4].allSatisfy({ $0 == 0 }) {
                // CCID time-extension: wait for the actual response.
                replyBytes.removeAll()
                return
            }
            let payloadLength = Int(bytes[1]) | (Int(bytes[2]) << 8) | (Int(bytes[3]) << 16) | (Int(bytes[4]) << 24)
            guard payloadLength <= 65535 else {
                invalidateTransport(.apduMalformed)
                return
            }
            replyExpected = 10 + payloadLength
        }
        guard let expected = replyExpected else { return }
        if replyBytes.count > expected {
            invalidateTransport(.apduMalformed)
        } else if replyBytes.count == expected {
            finishReply(.success(replyBytes))
        }
    }

    /// ESTKme-RED CCID firmware can request a short, optional continuation
    /// after its initial claim control exchange. The reply passed here is the
    /// complete CCID frame returned by `exchangeRaw`.
    private func ccidClaimContinuationFrame(for response: Data) -> Data? {
        let headerLength = 10
        guard response.count >= headerLength else { return nil }
        let payloadLength = Int(response[1])
            | (Int(response[2]) << 8)
            | (Int(response[3]) << 16)
            | (Int(response[4]) << 24)
        guard payloadLength >= 2, response.count >= headerLength + payloadLength,
              response[headerLength] == 0x61 else {
            return nil
        }

        // CCID XfrBlock carrying: FF FF FF 01 <continuation length>.
        return Data([0x6f, 5, 0, 0, 0, 0, 0, 0, 0, 0,
                     0xff, 0xff, 0xff, 1, response[headerLength + 1]])
    }

    // MARK: - Legacy Response Reassembly

    private func handleLegacyNotification(_ data: Data) {
        replyBytes.append(data)
        if replyExpected == nil {
            let headerLength = 3
            guard replyBytes.count >= headerLength else { return }
            let bytes = [UInt8](replyBytes)
            replyExpected = Int(bytes[1]) | (Int(bytes[2]) << 8)
            replyBytes.removeFirst(3)
        }
        guard let expected = replyExpected else { return }
        if replyBytes.count > expected {
            invalidateTransport(.apduMalformed)
        } else if replyBytes.count == expected {
            finishReply(.success(replyBytes))
        }
    }

    // MARK: - Reconnect

    private func scheduleReconnect() {
        guard CBManager.authorization == .allowedAlways else {
            if CBManager.authorization != .notDetermined { failBluetoothAuthorization() }
            return
        }
        guard !manualDisconnect, let id = desiredID, central?.state == .poweredOn else { return }
        phaseDeadline.cancel()
        reconnectWork?.cancel()
        reconnectAttempt = min(reconnectAttempt + 1, 5)
        let delay = min(30.0, Double(1 << (reconnectAttempt - 1)))
        state = .reconnecting
        detail = "蓝牙断开，\(Int(delay)) 秒后重连…"
        let work = DispatchWorkItem { [weak self] in
            guard let self, !self.manualDisconnect, self.desiredID == id else { return }
            guard CBManager.authorization == .allowedAlways else {
                if CBManager.authorization != .notDetermined { self.failBluetoothAuthorization() }
                return
            }
            self.startConnectionAttempt(id, detail: "正在重新连接 \(self.devices[id] ?? "蓝牙读卡器")…")
        }
        reconnectWork = work
        DispatchQueue.main.asyncAfter(deadline: .now() + delay, execute: work)
        ReaderOperationLogger.write(module: "bluetooth", action: "reconnect", state: "scheduled", result: "pending")
    }

    // MARK: - CBCentralManagerDelegate

    func centralManagerDidUpdateState(_ central: CBCentralManager) {
        // A retired central can still deliver a delayed callback after a
        // timeout or sheet dismissal. Only the current attempt owns the UI.
        guard self.central === central || (self.central == nil && centralInitializationPending) else { return }
        // The retained central can report a late permission result after the
        // sheet closes; it must not start discovery until scan() is requested.
        guard scanRequested || desiredID != nil || connectedID != nil || centralInitializationPending else { return }
        if CBManager.authorization == .notDetermined {
            centralInitializationPending = true
            deferredCentralStateCallback = true
            awaitCentralInitialization()
            return
        }
        if CBManager.authorization != .allowedAlways {
            failBluetoothAuthorization()
            return
        }
        deferredCentralStateCallback = false
        if centralInitializationPending && (central.state == .unknown || central.state == .resetting) {
            awaitCentralInitialization()
            return
        }
        centralInitializationPending = false
        centralInitializationDeadline.cancel()
        switch central.state {
        case .poweredOn:
            errorCode = nil
            if connectedID == nil {
                if let id = desiredID, peripherals[id] != nil {
                    startConnectionAttempt(id, detail: "蓝牙已恢复，正在重新连接…")
                    return
                }
            }
            if scanRequested && connectedID == nil && desiredID == nil {
                if let stored = UserDefaults.standard.string(forKey: preferredReaderKey),
                   let id = UUID(uuidString: stored),
                   let peripheral = central.retrievePeripherals(withIdentifiers: [id]).first {
                    peripherals[id] = peripheral
                    latestDiscoveredPeripherals[id] = peripheral
                    advertisedPeripheralIDs.remove(id)
                    let name = peripheral.name ?? ""
                    devices[id] = name.isEmpty ? "eSIM Reader" : name
                    protocols[id] = detectProtocol(name: name)
                    connect(id)
                    return
                }
                state = .scanning
                detail = "正在扫描蓝牙读卡器…"
                clearDiscoveryCache()
                beginDiscovery(on: central)
            }
        case .unauthorized:
            failBluetoothAuthorization()
        case .poweredOff:
            finishReply(.failure(BluetoothReaderTransportError(code: .poweredOff)))
            connectedID = nil
            notifyCharacteristic = nil
            writeCharacteristic = nil
            fail(.poweredOff, "请开启 Mac 蓝牙")
        case .unsupported:
            finishReply(.failure(BluetoothReaderTransportError(code: .unsupported)))
            connectedID = nil
            notifyCharacteristic = nil
            writeCharacteristic = nil
            fail(.unsupported, "本机不支持蓝牙低功耗")
        default: detail = "等待蓝牙就绪…"
        }
    }

    func centralManager(_ central: CBCentralManager, didDiscover peripheral: CBPeripheral,
                        advertisementData: [String: Any], rssi RSSI: NSNumber) {
        let id = peripheral.identifier
        // CoreBluetooth's cached peripheral.name can lag a newer advertised
        // local name. Later name-less packets must not flip the UI back or
        // downgrade a previously resolved APDU framing protocol.
        let advertisedName = (advertisementData[CBAdvertisementDataLocalNameKey] as? String)
            .flatMap { $0.isEmpty ? nil : $0 }
        let stagedAdvertisedName = stagedReaders[id].flatMap { $0.nameIsAdvertised ? $0.name : nil }
        let knownAdvertisedName = stagedAdvertisedName
            ?? (readersWithAdvertisedName.contains(id) ? devices[id] : nil)
        let cachedName = peripheral.name.flatMap { $0.isEmpty ? nil : $0 }
        let name = advertisedName ?? knownAdvertisedName ?? cachedName ?? devices[id] ?? ""
        let normalized = name.lowercased()
        let services = Set((advertisementData[CBAdvertisementDataServiceUUIDsKey] as? [CBUUID]) ?? [])

        // Match NekokoLPA2: check for device names containing estkme, esim_writer,
        // beesim, or 9esim; also match ESTKme service UUID 0x4553.
        let nameMatches = normalized.contains("estkme")
            || normalized.contains("estkred")
            || normalized.contains("beesim")
            || normalized.contains("9esim")
            || normalized.contains("esim_writer")
            || normalized.contains("simlink")
        let serviceMatches = services.contains(estkServiceUUID) || services.contains(beeSimServiceUUID)
        guard nameMatches || serviceMatches else { return }

        // Keep a newly discovered object for the next connection attempt,
        // without replacing the object bound to an active GATT/APDU exchange.
        latestDiscoveredPeripherals[id] = peripheral
        advertisedPeripheralIDs.insert(id)
        advertisedServices[id] = services
        let displayName = name.isEmpty ? "eSIM Reader" : name
        // A connection attempt pins its protocol before GATT discovery. All
        // later same-UUID advertisements, even before claim, belong to the
        // next attempt rather than the active response parser.
        if id == connectedID || id == desiredID {
            let knownName = stagedReaders[id]?.name ?? devices[id]
            let canRefineMetadata = nameMatches
                && (advertisedName != nil || knownName == nil || knownName == "eSIM Reader")
            let nextName = canRefineMetadata ? displayName : (knownName ?? displayName)
            let nextProtocol = canRefineMetadata
                ? detectProtocol(name: nextName)
                : (stagedReaders[id]?.proto ?? protocols[id] ?? detectProtocol(name: nextName))
            stagedReaders[id] = StagedReader(name: nextName,
                                              proto: nextProtocol,
                                              nameIsAdvertised: (canRefineMetadata && advertisedName != nil)
                                                  || stagedReaders[id]?.nameIsAdvertised == true
                                                  || readersWithAdvertisedName.contains(id))
            return
        }
        if id != desiredID { peripherals[id] = peripheral }
        if advertisedName != nil { readersWithAdvertisedName.insert(id) }
        protocols[id] = detectProtocol(name: displayName)
        if devices[id] != displayName {
            devices[id] = displayName
            bleDiagLog("didDiscover accepted: proto=\(protocols[id] ?? .legacy)")
            ReaderOperationLogger.write(module: "bluetooth", action: "discover", state: "found", result: "ok")
        }
    }

    func centralManager(_ central: CBCentralManager, didConnect peripheral: CBPeripheral) {
        guard desiredID == peripheral.identifier else {
            central.cancelPeripheralConnection(peripheral)
            bleDiagLog("didConnect ignored stale peripheral=\(peripheral.identifier)")
            return
        }
        guard peripherals[peripheral.identifier] === peripheral else {
            bleDiagLog("didConnect ignored stale peripheral object")
            // If the old object connected after its replacement was selected,
            // retire it without cancelling an already connected new object.
            if peripheral.state == .connected,
               peripherals[peripheral.identifier]?.state != .connected {
                central.cancelPeripheralConnection(peripheral)
            }
            peripheral.delegate = nil
            return
        }
        let name = devices[peripheral.identifier] ?? "?"
        let proto = protocols[peripheral.identifier] ?? .legacy
        reconnectWork?.cancel()
        reconnectWork = nil
        phaseDeadline.cancel()
        bleDiagLog("didConnect: name=\(name) proto=\(proto)")
        state = .discovering
        detail = "正在发现蓝牙读卡器服务…"
        peripheral.delegate = self
        armPhaseDeadline(
            for: peripheral.identifier,
            expectedState: .discovering,
            code: .serviceDiscoveryTimeout,
            message: "蓝牙连接已建立，但读取读卡器服务超时，稍后将自动重试")
        peripheral.discoverServices(nil)
    }

    func centralManager(_ central: CBCentralManager, didFailToConnect peripheral: CBPeripheral, error: Error?) {
        guard peripherals[peripheral.identifier] === peripheral,
              desiredID == peripheral.identifier, state == .connecting else {
            bleDiagLog("didFailToConnect ignored stale peripheral=\(peripheral.identifier)")
            return
        }
        phaseDeadline.cancel()
        bleDiagLog("didFailToConnect: error=\(error?.localizedDescription ?? "nil")")
        fail(.connectFailed, "连接失败：\(error?.localizedDescription ?? "设备未响应")")
        scheduleReconnect()
    }

    func centralManager(_ central: CBCentralManager, didDisconnectPeripheral peripheral: CBPeripheral, error: Error?) {
        guard peripherals[peripheral.identifier] === peripheral,
              desiredID == peripheral.identifier || connectedID == peripheral.identifier else { return }
        let retryAlreadyScheduled = !manualDisconnect && reconnectWork != nil
        phaseDeadline.cancel()
        if !retryAlreadyScheduled {
            reconnectWork?.cancel()
            reconnectWork = nil
        }
        finishReply(.failure(BluetoothReaderTransportError(code: .disconnected)))
        connectedID = nil
        notifyCharacteristic = nil
        writeCharacteristic = nil
        if retryAlreadyScheduled {
            state = .reconnecting
            detail = "蓝牙连接中断，等待自动重试…"
        } else {
            state = .disconnected
            detail = error.map { "蓝牙断开：\($0.localizedDescription)" } ?? "蓝牙已断开"
        }
        if !manualDisconnect {
            errorCode = .disconnected
            ReaderOperationLogger.write(module: "bluetooth", action: "disconnect", state: "unexpected", result: "error", errorCode: BluetoothReaderErrorCode.disconnected.rawValue)
            if !retryAlreadyScheduled {
                scheduleReconnect()
            }
        }
    }

    // MARK: - CBPeripheralDelegate

    func peripheral(_ peripheral: CBPeripheral, didDiscoverServices error: Error?) {
        guard peripherals[peripheral.identifier] === peripheral,
              desiredID == peripheral.identifier, state == .discovering,
              !notificationSetupStarted,
              peripheral.state == .connected else {
            bleDiagLog("didDiscoverServices ignored stale peripheral=\(peripheral.identifier) state=\(state)")
            return
        }
        phaseDeadline.cancel()
        let proto = protocols[peripheral.identifier] ?? .legacy
        if let error {
            bleDiagLog("didDiscoverServices ERROR: \(error.localizedDescription)")
            fail(.serviceMissing, "发现蓝牙服务失败：\(error.localizedDescription)")
            central?.cancelPeripheralConnection(peripheral)
            return
        }
        guard let services = peripheral.services, !services.isEmpty else {
            bleDiagLog("didDiscoverServices: NO SERVICES")
            fail(.serviceMissing, "设备没有暴露任何蓝牙服务")
            central?.cancelPeripheralConnection(peripheral)
            return
        }
        bleDiagLog("didDiscoverServices: \(services.count) services: \(services.map { $0.uuid.uuidString }) proto=\(proto)")

        // All protocols use full discovery + cross-service characteristic search.
        beeSimServicesToDiscover = services.count
        beeSimWriterHinted = nil
        beeSimNotifierHinted = nil
        beeSimWriterFallback = nil
        beeSimNotifierFallback = nil
        armPhaseDeadline(
            for: peripheral.identifier,
            expectedState: .discovering,
            code: .characteristicDiscoveryTimeout,
            message: "蓝牙读卡器服务已找到，但特征值发现超时，稍后将自动重试")
        for service in services {
            peripheral.discoverCharacteristics(nil, for: service)
        }
    }

    // BeeSIM characteristic search: since we discover ALL services (the
    // 9esim device may use non-standard service UUIDs), this callback fires
    // once per service. We accumulate candidates across calls and only
    // proceed when both TX and RX are found (or all services are done).
    private var beeSimWriterHinted: CBCharacteristic?
    private var beeSimNotifierHinted: CBCharacteristic?
    private var beeSimWriterFallback: CBCharacteristic?
    private var beeSimNotifierFallback: CBCharacteristic?
    private var beeSimServicesToDiscover: Int = 0

    func peripheral(_ peripheral: CBPeripheral, didDiscoverCharacteristicsFor service: CBService, error: Error?) {
        guard peripherals[peripheral.identifier] === peripheral,
              desiredID == peripheral.identifier, state == .discovering,
              !notificationSetupStarted,
              peripheral.state == .connected else {
            bleDiagLog("didDiscoverCharacteristics ignored stale peripheral=\(peripheral.identifier) state=\(state)")
            return
        }
        armPhaseDeadline(
            for: peripheral.identifier,
            expectedState: .discovering,
            code: .characteristicDiscoveryTimeout,
            message: "蓝牙读卡器特征值发现超时，稍后将自动重试")
        let proto = protocols[peripheral.identifier] ?? .legacy
        if let error {
            bleDiagLog("didDiscoverCharacteristics ERROR: svc=\(service.uuid.uuidString) \(error.localizedDescription)")
            fail(.characteristicMissing, "读取蓝牙特征失败：\(error.localizedDescription)")
            central?.cancelPeripheralConnection(peripheral)
            return
        }

        let charInfo = (service.characteristics ?? []).map { c -> String in
            let uuid = c.uuid.uuidString
            let props: [String] = [
                c.properties.contains(.read) ? "R" : "",
                c.properties.contains(.write) ? "W" : "",
                c.properties.contains(.writeWithoutResponse) ? "w" : "",
                c.properties.contains(.notify) ? "N" : "",
                c.properties.contains(.indicate) ? "I" : "",
            ].filter { !$0.isEmpty }
            return "\(uuid)(\(props.joined()))"
        }
        bleDiagLog("didDiscoverCharacteristics: svc=\(service.uuid.uuidString) chars=\(charInfo) proto=\(proto) remaining=\(beeSimServicesToDiscover-1)")

        switch proto {
        case .beeSim:
            // Accumulate AE01/AE02 hints and fallbacks across all services.
            for c in service.characteristics ?? [] {
                let uuidStr = c.uuid.uuidString.lowercased()
                let canWrite = c.properties.contains(.write) || c.properties.contains(.writeWithoutResponse)
                let canNotify = c.properties.contains(.notify) || c.properties.contains(.indicate)
                if uuidStr.contains("ae01") && canWrite && beeSimWriterHinted == nil {
                    beeSimWriterHinted = c
                } else if uuidStr.contains("ae02") && canNotify && beeSimNotifierHinted == nil {
                    beeSimNotifierHinted = c
                } else if canWrite && beeSimWriterFallback == nil {
                    beeSimWriterFallback = c
                } else if canNotify && beeSimNotifierFallback == nil {
                    beeSimNotifierFallback = c
                }
            }

            // Use the best candidates found so far.
            writeCharacteristic = beeSimWriterHinted ?? beeSimWriterFallback
            notifyCharacteristic = beeSimNotifierHinted ?? beeSimNotifierFallback

            guard let nc = notifyCharacteristic, let wc = writeCharacteristic else {
                if beeSimServicesToDiscover > 0 { beeSimServicesToDiscover -= 1 }
                if beeSimServicesToDiscover == 0 {
                    bleDiagLog("BeeSIM: FAILED no TX/RX found")
                    fail(.characteristicMissing, "设备不支持蓝牙 APDU 通道（未找到可写/可通知特征值）")
                    central?.cancelPeripheralConnection(peripheral)
                }
                return
            }
            bleDiagLog("BeeSIM: TX=\(wc.uuid.uuidString) RX=\(nc.uuid.uuidString)")
            beeSimWriterHinted = nil; beeSimNotifierHinted = nil
            beeSimWriterFallback = nil; beeSimNotifierFallback = nil
            beeSimServicesToDiscover = 0

        case .ccid:
            // ESTKme-RED CCID is intentionally strict. Unlike legacy readers,
            // it must bind to the known 4553 service and 544B/6D65 pair; a
            // generic write/notify fallback can silently select a control
            // channel that does not carry CCID frames.
            if notifyCharacteristic != nil, writeCharacteristic != nil {
                return
            }
            guard service.uuid == estkServiceUUID else {
                if beeSimServicesToDiscover > 0 { beeSimServicesToDiscover -= 1 }
                if beeSimServicesToDiscover == 0 {
                    bleDiagLog("CCID: FAILED missing ESTKme service 4553")
                    fail(.characteristicMissing, "ESTKme-RED 未找到 4553 / 544B / 6D65 蓝牙 APDU 通道")
                    central?.cancelPeripheralConnection(peripheral)
                }
                return
            }

            let characteristics = service.characteristics ?? []
            let writer = characteristics.first { characteristic in
                characteristic.uuid == estkWriteUUID
                    && (characteristic.properties.contains(.write)
                        || characteristic.properties.contains(.writeWithoutResponse))
            }
            let notifier = characteristics.first { characteristic in
                characteristic.uuid == estkNotifyUUID
                    && (characteristic.properties.contains(.notify)
                        || characteristic.properties.contains(.indicate))
            }
            guard let writer, let notifier else {
                bleDiagLog("CCID: FAILED missing exact 4553/544B/6D65 characteristics")
                fail(.characteristicMissing, "ESTKme-RED 未找到 4553 / 544B / 6D65 蓝牙 APDU 通道")
                central?.cancelPeripheralConnection(peripheral)
                return
            }
            writeCharacteristic = writer
            notifyCharacteristic = notifier
            beeSimServicesToDiscover = 0
            bleDiagLog("CCID: TX=\(writer.uuid.uuidString) RX=\(notifier.uuid.uuidString)")

        case .legacy:
            // Search for ESTKme characteristics (544b=notify, 6d65=write)
            // across all services. Also accept BeeSIM AE01/AE02 as fallback.
            for c in service.characteristics ?? [] {
                if c.uuid == estkNotifyUUID && beeSimNotifierHinted == nil {
                    beeSimNotifierHinted = c
                } else if c.uuid == estkWriteUUID && beeSimWriterHinted == nil {
                    beeSimWriterHinted = c
                } else {
                    let uuidStr = c.uuid.uuidString.lowercased()
                    let canWrite = c.properties.contains(.write) || c.properties.contains(.writeWithoutResponse)
                    let canNotify = c.properties.contains(.notify) || c.properties.contains(.indicate)
                    if uuidStr.contains("ae01") && canWrite && beeSimWriterHinted == nil {
                        beeSimWriterHinted = c
                    } else if uuidStr.contains("ae02") && canNotify && beeSimNotifierHinted == nil {
                        beeSimNotifierHinted = c
                    } else if canWrite && beeSimWriterFallback == nil {
                        beeSimWriterFallback = c
                    } else if canNotify && beeSimNotifierFallback == nil {
                        beeSimNotifierFallback = c
                    }
                }
            }
            writeCharacteristic = beeSimWriterHinted ?? beeSimWriterFallback
            notifyCharacteristic = beeSimNotifierHinted ?? beeSimNotifierFallback

            guard let nc = notifyCharacteristic, let wc = writeCharacteristic else {
                if beeSimServicesToDiscover > 0 { beeSimServicesToDiscover -= 1 }
                if beeSimServicesToDiscover == 0 {
                    bleDiagLog("legacy/ccid: FAILED no 544b/6d65 or AE01/AE02 or any W/N found")
                    fail(.characteristicMissing, "设备不支持蓝牙 APDU 通道（未找到 544b/6d65 或可写/可通知特征值）")
                    central?.cancelPeripheralConnection(peripheral)
                }
                return
            }
            bleDiagLog("legacy/ccid: TX=\(wc.uuid.uuidString) RX=\(nc.uuid.uuidString)")
            beeSimWriterHinted = nil; beeSimNotifierHinted = nil
            beeSimWriterFallback = nil; beeSimNotifierFallback = nil
            beeSimServicesToDiscover = 0
        }

        guard let nc = notifyCharacteristic, writeCharacteristic != nil else {
            return
        }
        // Other services may complete after the APDU channel was selected.
        // They cannot replace it or restart a GATT deadline during card I/O.
        notificationSetupStarted = true
        armPhaseDeadline(
            for: peripheral.identifier,
            expectedState: .discovering,
            code: .notificationTimeout,
            message: "蓝牙读卡器通知通道未就绪，稍后将自动重试")
        peripheral.setNotifyValue(true, for: nc)
    }

    func peripheral(_ peripheral: CBPeripheral, didUpdateNotificationStateFor characteristic: CBCharacteristic, error: Error?) {
        guard peripherals[peripheral.identifier] === peripheral,
              desiredID == peripheral.identifier,
              state == .discovering || state == .connected,
              peripheral.state == .connected,
              let selectedNotify = notifyCharacteristic,
              characteristic === selectedNotify else { return }
        phaseDeadline.cancel()
        bleDiagLog("didUpdateNotificationState: char=\(characteristic.uuid.uuidString) notifying=\(characteristic.isNotifying) error=\(error?.localizedDescription ?? "nil")")
        guard error == nil, characteristic.isNotifying else {
            invalidateTransport(.notificationsFailed)
            return
        }

        // Duplicate notify acknowledgements must not repeat claim/power-on,
        // including while a previous handshake APDU is still outstanding.
        guard state == .discovering, !cardHandshakeStarted else { return }
        cardHandshakeStarted = true

        let id = peripheral.identifier
        let proto = protocols[id] ?? .legacy

        switch proto {

        case .beeSim:
            // BeeSIM power-up: A0 3E 04 00 00 (raises TX power to level 4)
            let powerUpFrame = Data([0xA0, 0x3E, 0x04, 0x00, 0x00])
            exchangeRaw(powerUpFrame) { [weak self] result in
                guard let self else { return }
                switch result {
                case .failure(let error):
                    self.fail(.connectFailed, "BeeSIM 初始化失败：\(error.localizedDescription)")
                    self.central?.cancelPeripheralConnection(peripheral)
                case .success:
                    UserDefaults.standard.set(peripheral.identifier.uuidString, forKey: self.preferredReaderKey)
                    self.reconnectAttempt = 0
                    self.errorCode = nil
                    self.detail = "BeeSIM 蓝牙卡片通道已就绪"
                    self.state = .connected
                    // Publish the selected ID last: the UI treats it as the
                    // signal that card power-on and APDU setup are complete.
                    self.connectedID = peripheral.identifier
                    ReaderOperationLogger.write(module: "bluetooth", action: "connect", state: "card_powered", result: "ok")
                }
            }
        case .ccid:
            // CCID claim + power-on
            let claimFrame = Data([0x6f, 7, 0, 0, 0, 0, 0, 0, 0, 0, 0xff, 0xff, 0xff, 0, 2, 0, 0])
            exchangeRaw(claimFrame) { [weak self] claim in
                guard let self else { return }
                switch claim {
                case .failure(let error):
                    self.fail(.connectFailed, "ESTK 占用失败：\(error.localizedDescription)")
                    self.central?.cancelPeripheralConnection(peripheral)
                case .success(let claimResponse):
                    let powerOn: () -> Void = { [weak self] in
                        guard let self else { return }
                        let powerFrame = Data([0x62, 1, 0, 0, 0, 0, 0, 0, 0, 0, 1])
                        self.exchangeRaw(powerFrame) { [weak self] power in
                            guard let self else { return }
                            switch power {
                            case .failure(let error):
                                self.fail(.connectFailed, "ESTK 上电失败：\(error.localizedDescription)")
                                self.central?.cancelPeripheralConnection(peripheral)
                            case .success:
                                self.completeConnection(peripheral)
                            }
                        }
                    }

                    guard let continuation = self.ccidClaimContinuationFrame(for: claimResponse) else {
                        powerOn()
                        return
                    }
                    self.bleDiagLog("CCID: applying optional claim continuation")
                    self.exchangeRaw(continuation) { [weak self] continuationResult in
                        guard let self else { return }
                        // NekokoLPA2 treats this negotiation as optional. Keep
                        // the established first-claim behavior as the fallback,
                        // while using the continuation whenever firmware asks.
                        if case .failure(let error) = continuationResult {
                            self.bleDiagLog("CCID: optional claim continuation failed: \(error.localizedDescription)")
                        }
                        powerOn()
                    }
                }
            }
        case .legacy:
            // Legacy claim + power-on
            let claimFrame = Data([2, 6, 0, 0x45, 0x53, 0x54, 0x4b, 0x6d, 0x65])
            exchangeRaw(claimFrame) { [weak self] claim in
                guard let self else { return }
                switch claim {
                case .failure(let error):
                    self.fail(.connectFailed, "ESTK 占用失败：\(error.localizedDescription)")
                    self.central?.cancelPeripheralConnection(peripheral)
                case .success:
                    let powerFrame = Data([3, 2, 0, 1, 1])
                    self.exchangeRaw(powerFrame) { [weak self] power in
                        guard let self else { return }
                        switch power {
                        case .failure(let error):
                            self.fail(.connectFailed, "ESTK 上电失败：\(error.localizedDescription)")
                            self.central?.cancelPeripheralConnection(peripheral)
                        case .success:
                            self.completeConnection(peripheral)
                        }
                    }
                }
            }
        }
    }

    private func completeConnection(_ peripheral: CBPeripheral) {
        phaseDeadline.cancel()
        UserDefaults.standard.set(peripheral.identifier.uuidString, forKey: preferredReaderKey)
        reconnectAttempt = 0
        errorCode = nil
        detail = "蓝牙卡片通道已就绪"
        state = .connected
        connectedID = peripheral.identifier
        ReaderOperationLogger.write(module: "bluetooth", action: "connect", state: "card_powered", result: "ok")
    }
}

struct BluetoothReaderTransportError: LocalizedError {
    let code: BluetoothReaderErrorCode
    var errorDescription: String? { code.rawValue }
}

/// The UI consumes this service, not CBCentralManager or CBPeripheral.
final class BluetoothReaderService: ObservableObject {
    private let manager: BluetoothReaderManager
    private var subscription: AnyCancellable?

    init(manager: BluetoothReaderManager = .shared) {
        self.manager = manager
        subscription = manager.objectWillChange.sink { [weak self] _ in self?.objectWillChange.send() }
    }

    var statusText: String { manager.detail }
    var devices: [UUID: String] { manager.devices }
    var connectedID: UUID? { manager.connectedID }
    var errorCode: BluetoothReaderErrorCode? { manager.errorCode }
    func scan() { manager.scan() }
    func stopScan() { manager.stopScan() }
    func connect(_ id: UUID) { manager.connect(id) }
    func disconnect() { manager.disconnect() }
}

private enum ReaderOperationLogger {
    static func write(module: String, action: String, state: String, result: String, errorCode: String? = nil) {
        var event: [String: String] = [
            "timestamp": ISO8601DateFormatter().string(from: Date()),
            "module": module, "action": action, "state": state, "result": result
        ]
        if let errorCode { event["errorCode"] = errorCode }
        guard let bytes = try? JSONSerialization.data(withJSONObject: event),
              let line = String(data: bytes, encoding: .utf8) else { return }
        NSLog("%@", line)
    }
}
