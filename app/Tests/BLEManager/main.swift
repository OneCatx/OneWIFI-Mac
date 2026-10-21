import Foundation

private struct CheckFailure: Error, CustomStringConvertible {
    let description: String
}

private func expect(_ condition: @autoclosure () -> Bool, _ message: String) throws {
    if !condition() { throw CheckFailure(description: message) }
}

private struct Reader {
    let peripheral: CBPeripheral
    let notify: CBCharacteristic
    let writer: CBCharacteristic
    let service: CBService
    let unrelatedService: CBService
    let response: Data

    init(name: String = "ESTKme-RED", peripheral existingPeripheral: CBPeripheral? = nil,
         writerProperties: CBCharacteristicProperties = .writeWithoutResponse) {
        peripheral = existingPeripheral ?? CBPeripheral(name: name)
        if name == "BeeSIM" {
            notify = CBCharacteristic("AE02", .notify)
            writer = CBCharacteristic("AE01", .writeWithoutResponse)
            service = CBService("AE30", [notify, writer])
            response = Data([1, 1])
        } else {
            notify = CBCharacteristic("544B", .notify)
            writer = CBCharacteristic("6D65", writerProperties)
            service = CBService("4553", [notify, writer])
            response = name.lowercased().contains("estkme")
                ? Data([0x80, 0, 0, 0, 0, 0, 0, 0, 0, 0])
                : Data([0, 0, 0])
        }
        unrelatedService = CBService("180A", [CBCharacteristic("2A29", .read)])
        peripheral.services = [service, unrelatedService]
    }
}

private final class Fixture {
    let manager: BluetoothReaderManager
    let central: CBCentralManager
    let clock: BLETestQueue
    let reader: Reader

    init(name: String = "ESTKme-RED", writerProperties: CBCharacteristicProperties = .writeWithoutResponse) {
        BLETestQueue.main = BLETestQueue()
        BLETestDefaults.shared.reset()
        BLETestDiagnostics.reset()
        clock = BLETestQueue.main
        manager = BluetoothReaderManager()
        central = CBCentralManager(delegate: manager, queue: clock)
        manager.central = central
        reader = Reader(name: name, writerProperties: writerProperties)
        discover(reader)
        manager.connect(reader.peripheral.identifier)
    }

    func discover(_ reader: Reader) {
        manager.centralManager(central, didDiscover: reader.peripheral,
                               advertisementData: [:], rssi: -50)
    }

    func connected(_ selected: Reader? = nil) {
        let selected = selected ?? reader
        selected.peripheral.state = .connected
        manager.centralManager(central, didConnect: selected.peripheral)
    }

    func services(_ selected: Reader? = nil) {
        let selected = selected ?? reader
        manager.peripheral(selected.peripheral, didDiscoverServices: nil)
    }

    func characteristics(_ selected: Reader? = nil) {
        let selected = selected ?? reader
        manager.peripheral(selected.peripheral, didDiscoverCharacteristicsFor: selected.service, error: nil)
    }

    func notificationReady(_ selected: Reader? = nil) {
        let selected = selected ?? reader
        selected.notify.isNotifying = true
        manager.peripheral(selected.peripheral, didUpdateNotificationStateFor: selected.notify, error: nil)
    }

    func beginHandshake(_ selected: Reader? = nil) {
        connected(selected)
        services(selected)
        characteristics(selected)
        notificationReady(selected)
    }

    func reply(_ selected: Reader? = nil) {
        let selected = selected ?? reader
        selected.notify.value = selected.response
        manager.peripheral(selected.peripheral, didUpdateValueFor: selected.notify, error: nil)
    }

    func acknowledgeWrite(_ selected: Reader? = nil, error: Error? = nil) {
        let selected = selected ?? reader
        manager.peripheral(selected.peripheral, didWriteValueFor: selected.writer, error: error)
    }

    func finishHandshake(_ selected: Reader? = nil) throws {
        for _ in 0..<8 where manager.pendingReply != nil {
            if manager.writeAwaitingResponse { acknowledgeWrite(selected) }
            if manager.pendingReply != nil { reply(selected) }
        }
        try expect(manager.state == .connected, "handshake did not reach connected: \(manager.state)")
    }

    func disconnected(_ selected: Reader? = nil) {
        let selected = selected ?? reader
        selected.peripheral.state = .disconnected
        manager.centralManager(central, didDisconnectPeripheral: selected.peripheral, error: nil)
    }
}

@main
private enum BLEManagerTests {
    static func initialCentralTimeoutAllowsRetry() throws {
        BLETestQueue.main = BLETestQueue()
        let manager = BluetoothReaderManager()
        manager.scan()
        guard let firstCentral = manager.central else {
            throw CheckFailure(description: "first scan did not create a central")
        }
        try expect(manager.detail == "正在检查蓝牙…", "first scan did not show initialization status")
        BLETestQueue.main.advance(9)
        try expect(manager.errorCode == nil, "central initialization timed out early")
        BLETestQueue.main.advance(1)
        try expect(manager.errorCode == .initializationTimeout && manager.state == .failed,
                   "missing initial callback did not produce a bounded failure")
        try expect(manager.central == nil && !manager.isScanning,
                   "timed-out central was retained as an active scan")
        manager.scan()
        guard let replacement = manager.central else {
            throw CheckFailure(description: "retry did not create a central")
        }
        try expect(replacement !== firstCentral, "retry reused the timed-out central")
        manager.centralManagerDidUpdateState(firstCentral)
        try expect(manager.detail == "正在检查蓝牙…", "retired central changed retry status")
        manager.centralManagerDidUpdateState(replacement)
        BLETestQueue.main.advance(11)
        try expect(manager.state == .scanning && manager.errorCode == nil,
                   "replacement callback did not cancel initialization deadline")
    }

    static func initialCentralCallbackCancelsTimeout() throws {
        for state in [CBManagerState.poweredOn, .unauthorized] {
            BLETestQueue.main = BLETestQueue()
            let manager = BluetoothReaderManager()
            manager.scan()
            guard let central = manager.central else {
                throw CheckFailure(description: "scan did not create a central")
            }
            central.state = state
            manager.centralManagerDidUpdateState(central)
            BLETestQueue.main.advance(11)
            switch state {
            case .poweredOn:
                try expect(manager.state == .scanning && manager.errorCode == nil,
                           "powered-on callback did not start discovery")
            case .unauthorized:
                try expect(manager.state == .failed && manager.errorCode == .unauthorized,
                           "authorization failure was replaced by initialization timeout")
            default: break
            }
        }
    }

    static func closingDuringCentralInitializationCancelsTimeout() throws {
        for disconnect in [false, true] {
            BLETestQueue.main = BLETestQueue()
            let manager = BluetoothReaderManager()
            manager.scan()
            guard let central = manager.central else {
                throw CheckFailure(description: "scan did not create a central")
            }
            if disconnect { manager.disconnect() }
            else { manager.stopScan() }
            let expectedState: BluetoothReaderState = disconnect ? .disconnected : .idle
            try expect(manager.central == nil && manager.state == expectedState,
                       "closing retained an initializing central")
            BLETestQueue.main.advance(11)
            manager.centralManagerDidUpdateState(central)
            try expect(manager.state == expectedState && manager.errorCode == nil,
                       "cancelled deadline or stale callback changed closed state")
        }
    }

    static func permissionPromptOutlivesInitializationDeadline() throws {
        CBManager.authorization = .notDetermined
        defer { CBManager.authorization = .allowedAlways }
        for callbackBeforeGrant in [CBManagerState.unknown, .poweredOn] {
            BLETestQueue.main = BLETestQueue()
            CBManager.authorization = .notDetermined
            let manager = BluetoothReaderManager()
            manager.scan()
            guard let central = manager.central else {
                throw CheckFailure(description: "scan did not create a central")
            }
            try expect(manager.detail.contains("允许 OneWIFI 使用蓝牙"),
                       "undetermined authorization did not show permission guidance")
            central.state = callbackBeforeGrant
            manager.centralManagerDidUpdateState(central)
            BLETestQueue.main.advance(26)
            try expect(manager.central === central && manager.errorCode == nil && !manager.isScanning && central.scanCount == 0,
                       "permission prompt retired or scanned the central before authorization")

            CBManager.authorization = .allowedAlways
            if callbackBeforeGrant == .unknown {
                central.state = .poweredOn
                manager.centralManagerDidUpdateState(central)
            } else {
                // The earlier powered-on callback is enough once consent is granted.
                BLETestQueue.main.advance(1)
            }
            BLETestQueue.main.advance(11)
            try expect(manager.state == .scanning && manager.errorCode == nil && central.scanCount == 1,
                       "late grant did not resume discovery on the original central")
        }

        BLETestQueue.main = BLETestQueue()
        CBManager.authorization = .notDetermined
        let deniedManager = BluetoothReaderManager()
        deniedManager.scan()
        guard let deniedCentral = deniedManager.central else {
            throw CheckFailure(description: "denied scan did not create a central")
        }
        deniedCentral.state = .poweredOn
        deniedManager.centralManagerDidUpdateState(deniedCentral)
        CBManager.authorization = .denied
        BLETestQueue.main.advance(1)
        try expect(deniedManager.errorCode == .unauthorized && deniedCentral.scanCount == 0,
                   "denial after a deferred powered-on callback started a scan")
        deniedManager.scan()
        try expect(deniedManager.errorCode == .unauthorized && deniedCentral.scanCount == 0,
                   "explicit scan ignored denied authorization on a powered-on central")
    }

    static func grantWithoutStateCallbackStillTimesOut() throws {
        BLETestQueue.main = BLETestQueue()
        CBManager.authorization = .notDetermined
        defer { CBManager.authorization = .allowedAlways }
        let manager = BluetoothReaderManager()
        manager.scan()
        guard let firstCentral = manager.central else {
            throw CheckFailure(description: "scan did not create a central")
        }
        BLETestQueue.main.advance(25)
        CBManager.authorization = .allowedAlways
        BLETestQueue.main.advance(1)
        try expect(manager.detail == "正在检查蓝牙…", "granted authorization kept permission guidance")
        BLETestQueue.main.advance(9)
        try expect(manager.errorCode == nil, "authorized central timed out early")
        BLETestQueue.main.advance(1)
        try expect(manager.state == .failed && manager.errorCode == .initializationTimeout && manager.central == nil,
                   "missing post-grant state callback did not allow a bounded retry")

        manager.scan()
        guard let replacement = manager.central else {
            throw CheckFailure(description: "retry did not create a central")
        }
        try expect(replacement !== firstCentral, "retry reused the timed-out central")
        firstCentral.state = .poweredOn
        manager.centralManagerDidUpdateState(firstCentral)
        try expect(replacement.scanCount == 0 && manager.state == .idle,
                   "retired central callback started a stale scan")
        replacement.state = .poweredOn
        manager.centralManagerDidUpdateState(replacement)
        try expect(manager.state == .scanning && replacement.scanCount == 1,
                   "replacement central did not resume discovery")
    }

    static func sheetCloseDuringPermissionPromptKeepsGrantForNextScan() throws {
        BLETestQueue.main = BLETestQueue()
        CBManager.authorization = .notDetermined
        defer { CBManager.authorization = .allowedAlways }
        let manager = BluetoothReaderManager()
        manager.scan()
        guard let central = manager.central else {
            throw CheckFailure(description: "scan did not create a central")
        }
        central.state = .poweredOn
        manager.centralManagerDidUpdateState(central)
        manager.stopScan()
        try expect(manager.central === central && manager.state == .idle && !manager.isScanning,
                   "sheet close discarded the pending permission central")

        BLETestQueue.main.advance(26)
        CBManager.authorization = .allowedAlways
        central.state = .poweredOn
        manager.centralManagerDidUpdateState(central)
        try expect(manager.state == .idle && manager.errorCode == nil && central.scanCount == 0,
                   "late grant started discovery after sheet close")
        manager.scan()
        try expect(manager.central === central && manager.state == .scanning && central.scanCount == 1,
                   "next explicit scan did not use the granted central")

        let connected = Fixture()
        connected.central.state = .poweredOff
        connected.manager.centralManagerDidUpdateState(connected.central)
        connected.manager.stopScan()
        connected.central.state = .poweredOn
        connected.manager.centralManagerDidUpdateState(connected.central)
        try expect(connected.manager.state == .connecting && connected.central.connections.count == 2,
                   "sheet close blocked selected reader reconnect after Bluetooth recovered")
    }

    static func denialBlocksQueuedReconnectAndAllowsLaterRecovery() throws {
        defer { CBManager.authorization = .allowedAlways }
        for deliverStateCallback in [false, true] {
            CBManager.authorization = .allowedAlways
            let fixture = Fixture()
            let manager = fixture.manager
            let id = fixture.reader.peripheral.identifier
            manager.centralManager(fixture.central, didFailToConnect: fixture.reader.peripheral, error: nil)
            try expect(manager.state == .reconnecting && manager.reconnectWork != nil,
                       "failed connection did not queue a retry")

            CBManager.authorization = .denied
            if deliverStateCallback { manager.centralManagerDidUpdateState(fixture.central) }
            fixture.clock.advance(2)
            try expect(manager.state == .failed && manager.errorCode == .unauthorized,
                       "denied retry did not report Bluetooth authorization")
            try expect(manager.reconnectWork == nil && fixture.central.connections.count == 1,
                       "queued retry connected after authorization was denied")
            manager.connect(id)
            try expect(manager.desiredID == id && fixture.central.connections.count == 1,
                       "explicit denied connect mutated the selected link or connected")

            CBManager.authorization = .allowedAlways
            manager.centralManagerDidUpdateState(fixture.central)
            try expect(manager.state == .connecting && fixture.central.connections.count == 2,
                       "authorized radio recovery did not reconnect the selected reader")
        }
    }

    static func lateServicesAndDuplicateNotify() throws {
        for name in ["ESTKme-RED", "ESTKme Red:205C36914730", "BeeSIM", "estkred"] {
            let fixture = Fixture(name: name)
            fixture.beginHandshake()
            let reader = fixture.reader
            let manager = fixture.manager
            try expect(manager.pendingReply != nil, "\(name): handshake APDU was not started")
            let writes = reader.peripheral.writes.count
            fixture.clock.advance(1)
            // All services were requested in parallel. Responses, including
            // failures, may arrive after the selected notification is ready.
            manager.peripheral(reader.peripheral, didDiscoverServices: nil)
            manager.peripheral(reader.peripheral, didDiscoverCharacteristicsFor: reader.unrelatedService, error: nil)
            manager.peripheral(reader.peripheral, didDiscoverCharacteristicsFor: reader.unrelatedService,
                               error: NSError(domain: "late-service", code: 1))
            fixture.notificationReady()
            try expect(reader.peripheral.writes.count == writes, "\(name): duplicate notify started another APDU")
            try expect(manager.notifyCharacteristic === reader.notify, "\(name): late service replaced the selected channel")
            fixture.clock.advance(59)
            try expect(fixture.central.cancellations.isEmpty, "\(name): late callback cancelled a healthy 120s handshake APDU")
            try expect(manager.pendingReply != nil, "\(name): handshake reply disappeared before 120s")
            try fixture.finishHandshake()
            fixture.clock.advance(121)
            try expect(manager.state == .connected, "\(name): completed handshake leaked a deadline")
        }
    }

    static func characteristicWriteModesAndResponsePacing() throws {
        let cases: [(CBCharacteristicProperties, CBCharacteristicWriteType)] = [
            (.write, .withResponse),
            ([.write, .writeWithoutResponse], .withResponse),
            (.writeWithoutResponse, .withoutResponse)
        ]
        for (properties, expectedType) in cases {
            let fixture = Fixture(writerProperties: properties)
            fixture.beginHandshake()
            try expect(fixture.reader.peripheral.writeTypes.first == expectedType,
                       "writer properties \(properties.rawValue) selected the wrong GATT write type")
            try fixture.finishHandshake()
            try expect(fixture.reader.peripheral.writeTypes.allSatisfy { $0 == expectedType },
                       "claim/power-on changed GATT write type mid-session")

            guard properties == .write else { continue }
            let firstWrite = fixture.reader.peripheral.writes.count
            var result: Result<Data, Error>?
            fixture.manager.exchangeAPDU(Data(repeating: 0xA5, count: 500)) { result = $0 }
            try expect(fixture.reader.peripheral.writes.count == firstWrite + 1,
                       "with-response transport did not wait after its first chunk")
            fixture.acknowledgeWrite()
            try expect(fixture.reader.peripheral.writes.count == firstWrite + 2,
                       "second with-response chunk was not sent after acknowledgement")
            fixture.acknowledgeWrite()
            try expect(fixture.reader.peripheral.writes.count == firstWrite + 3,
                       "third with-response chunk was not sent after acknowledgement")
            fixture.acknowledgeWrite()
            let sent = Data(fixture.reader.peripheral.writes.suffix(3).flatMap { Array($0) })
            var expectedFrame = Data([0x6f, 0xf4, 0x01, 0, 0, 0, 0, 0, 0, 0])
            expectedFrame.append(Data(repeating: 0xA5, count: 500))
            try expect(sent == expectedFrame, "paced GATT chunks changed the CCID APDU bytes")
            fixture.reply()
            guard case .success(let response)? = result else {
                throw CheckFailure(description: "with-response APDU did not complete after CCID response")
            }
            try expect(response.isEmpty, "empty CCID APDU response changed")
        }

        let failedWrite = Fixture(writerProperties: .write)
        failedWrite.beginHandshake()
        let writeCount = failedWrite.reader.peripheral.writes.count
        failedWrite.acknowledgeWrite(error: NSError(domain: "gatt-write", code: 1))
        try expect(failedWrite.reader.peripheral.writes.count == writeCount,
                   "failed GATT write advanced the claim/power-on sequence")
        try expect(failedWrite.manager.pendingReply == nil && failedWrite.manager.connectedID == nil,
                   "failed GATT write retained a ready or pending session")
    }

    static func readyBeforeConnectedIDPublication() throws {
        for name in ["ESTKme-RED", "ESTKme Red:205C36914730", "BeeSIM", "estkred"] {
            let fixture = Fixture(name: name)
            fixture.beginHandshake()
            try fixture.finishHandshake()
            try expect(fixture.manager.connectedID == fixture.reader.peripheral.identifier,
                       "\(name): successful APDU handshake did not publish reader ID")
            try expect(!BLETestDiagnostics.lines.contains("connected_published_before_ready"),
                       "\(name): reader ID was published before the GATT/APDU link was ready")
        }
    }

    static func notificationLoss() throws {
        for callbackHasError in [false, true] {
            let fixture = Fixture()
            fixture.beginHandshake()
            try fixture.finishHandshake()
            var results: [BluetoothReaderErrorCode] = []
            fixture.manager.exchangeRaw(Data([0x6f])) { result in
                if case .failure(let error) = result,
                   let error = error as? BluetoothReaderTransportError { results.append(error.code) }
            }
            fixture.reader.notify.isNotifying = callbackHasError
            fixture.manager.peripheral(fixture.reader.peripheral,
                didUpdateNotificationStateFor: fixture.reader.notify,
                error: callbackHasError ? NSError(domain: "notify", code: 1) : nil)
            try expect(results == [.notificationsFailed], "notify loss must fail the pending APDU exactly once")
            try expect(fixture.manager.connectedID == nil, "notify loss still advertises a connected reader")
            try expect(fixture.central.cancellations.contains(fixture.reader.peripheral.identifier), "notify loss did not cancel transport")
            try expect(fixture.manager.notifyCharacteristic == nil, "notify loss retained the old notification channel")
            // No didDisconnect is delivered: recovery must not depend on it.
            fixture.clock.advance(1)
            try expect(fixture.central.connections.count == 2, "notify loss did not schedule recovery")
            fixture.beginHandshake()
            try fixture.finishHandshake()
            fixture.clock.advance(121)
            try expect(fixture.manager.state == .connected, "old APDU deadline invalidated the recovered connection")
            try expect(results.count == 1, "old APDU completion was delivered twice")
        }
    }

    static func supersededReaderCallbacks() throws {
        let fixture = Fixture()
        fixture.beginHandshake()
        try fixture.finishHandshake()
        let old = fixture.reader
        let replacement = Reader()
        fixture.discover(replacement)
        fixture.manager.connect(replacement.peripheral.identifier)
        // The replacement is still connecting while callbacks from the
        // previous peripheral are delivered in arbitrary order.
        fixture.manager.peripheral(old.peripheral, didDiscoverServices: nil)
        fixture.manager.peripheral(old.peripheral, didDiscoverCharacteristicsFor: old.service, error: nil)
        fixture.manager.peripheral(old.peripheral, didUpdateNotificationStateFor: old.notify, error: nil)
        fixture.manager.centralManager(fixture.central, didFailToConnect: old.peripheral, error: nil)
        fixture.disconnected(old)
        try expect(fixture.manager.state == .connecting, "old callback changed replacement connection state")
        fixture.clock.advance(10)
        fixture.beginHandshake(replacement)
        old.notify.isNotifying = false
        fixture.manager.peripheral(old.peripheral, didUpdateNotificationStateFor: old.notify, error: nil)
        fixture.reply(old)
        fixture.clock.advance(60)
        try expect(!fixture.central.cancellations.contains(replacement.peripheral.identifier), "old deadline/callback cancelled replacement APDU")
        try expect(fixture.manager.pendingReply != nil, "old notification consumed the replacement reply")
        try fixture.finishHandshake(replacement)
        try expect(fixture.manager.connectedID == replacement.peripheral.identifier, "replacement lost its identity")
        fixture.clock.advance(121)
        try expect(fixture.manager.state == .connected, "superseded deadline survived replacement")
    }

    static func phaseTimeoutsAndDelayedDisconnect() throws {
        let expected: [BluetoothReaderErrorCode] = [
            .connectionTimeout, .serviceDiscoveryTimeout,
            .characteristicDiscoveryTimeout, .notificationTimeout
        ]
        for phase in 0..<4 {
            let fixture = Fixture()
            if phase >= 1 { fixture.connected() }
            if phase >= 2 { fixture.services() }
            if phase >= 3 { fixture.characteristics() }
            fixture.clock.advance(29)
            try expect(fixture.central.cancellations.isEmpty, "phase \(phase) expired early")
            fixture.clock.advance(1)
            try expect(fixture.manager.errorCode == expected[phase], "phase \(phase) returned wrong timeout")
            try expect(fixture.central.cancellations.count == 1, "phase \(phase) did not cancel stalled link")
            try expect(fixture.manager.state == .reconnecting, "phase \(phase) did not schedule reconnect")
            fixture.disconnected()
            fixture.clock.advance(1)
            try expect(fixture.central.connections.count == 2, "late didDisconnect postponed/doubled retry")
            fixture.beginHandshake()
            try fixture.finishHandshake()
        }
    }

    static func manualDisconnectCancelsDeadlines() throws {
        for phase in 0..<5 {
            let fixture = Fixture()
            if phase >= 1 { fixture.connected() }
            if phase >= 2 { fixture.services() }
            if phase >= 3 { fixture.characteristics() }
            if phase >= 4 { fixture.notificationReady() }
            fixture.manager.disconnect()
            let cancellations = fixture.central.cancellations.count
            fixture.disconnected()
            fixture.clock.advance(200)
            try expect(fixture.manager.state == .disconnected, "manual disconnect left phase \(phase) in \(fixture.manager.state)")
            try expect(fixture.manager.desiredID == nil, "manual disconnect retained desired reader")
            try expect(fixture.central.connections.count == 1, "manual disconnect scheduled another connection")
            try expect(fixture.central.cancellations.count == cancellations, "cancelled phase/APDU deadline fired after manual disconnect")
            try expect(fixture.manager.pendingReply == nil, "manual disconnect retained an APDU")
        }
    }

    static func apduTimeoutIsolatedFromGATTDeadline() throws {
        let fixture = Fixture()
        fixture.beginHandshake()
        try fixture.finishHandshake()
        var results: [BluetoothReaderErrorCode] = []
        fixture.manager.exchangeRaw(Data([0x6f])) { result in
            if case .failure(let error) = result,
               let error = error as? BluetoothReaderTransportError { results.append(error.code) }
        }
        fixture.clock.advance(119)
        try expect(results.isEmpty && fixture.central.cancellations.isEmpty, "connected APDU was interrupted before 120s")
        fixture.clock.advance(1)
        try expect(results == [.apduTimeout], "120s APDU deadline did not fail exactly once")
        try expect(fixture.manager.connectedID == nil, "APDU timeout retained connected identity")
        try expect(fixture.manager.notifyCharacteristic == nil, "APDU timeout retained stale notification identity")
        fixture.clock.advance(1)
        let replacement = Reader(peripheral: fixture.reader.peripheral)
        fixture.beginHandshake(replacement)
        // The old characteristic object is not the selected object after a
        // new discovery, so its late bytes cannot complete the new request.
        let staleNotify = fixture.reader.notify
        staleNotify.value = fixture.reader.response
        let writes = fixture.reader.peripheral.writes.count
        fixture.manager.peripheral(fixture.reader.peripheral, didUpdateValueFor: staleNotify, error: nil)
        try expect(fixture.reader.peripheral.writes.count == writes, "stale APDU response advanced the replacement handshake")
        try fixture.finishHandshake(replacement)
        fixture.clock.advance(121)
        try expect(fixture.manager.state == .connected && results.count == 1, "old APDU completion affected recovered link")
    }

    static func channelRevokedBeforeContinuation() throws {
        let fixture = Fixture()
        fixture.beginHandshake()
        // Request CCID's optional claim continuation through the real parser.
        fixture.reader.notify.value = Data([0x80, 2, 0, 0, 0, 0, 0, 0, 0, 0, 0x61, 1])
        fixture.manager.peripheral(fixture.reader.peripheral, didUpdateValueFor: fixture.reader.notify, error: nil)
        let writes = fixture.reader.peripheral.writes.count
        fixture.reader.notify.isNotifying = false
        fixture.manager.peripheral(fixture.reader.peripheral, didUpdateNotificationStateFor: fixture.reader.notify, error: nil)
        try expect(fixture.reader.peripheral.writes.count == writes, "failed optional claim wrote power-on to revoked transport")
        try expect(fixture.manager.pendingReply == nil, "failed optional claim retained a replacement APDU")
        fixture.clock.advance(1)
        try expect(fixture.central.connections.count == 2, "failed optional claim lost recovery scheduling")
    }

    static func discoveryDeduplicatesDiagnosticsWithoutLosingReaders() throws {
        let fixture = Fixture()
        let selectedID = fixture.reader.peripheral.identifier
        BLETestDiagnostics.reset()
        let unrelated = Reader(name: "Unrelated Device")
        for _ in 0..<100 {
            fixture.discover(unrelated)
            fixture.discover(fixture.reader)
        }
        try expect(fixture.manager.devices[selectedID] == "ESTKme-RED", "duplicate advertisements changed selected reader")
        try expect(fixture.manager.devices[unrelated.peripheral.identifier] == nil, "unrelated advertisement became a reader")
        try expect(BLETestDiagnostics.lines.isEmpty, "duplicate or unrelated advertisements caused diagnostic disk writes")

        fixture.manager.centralManager(fixture.central, didDiscover: fixture.reader.peripheral,
                                       advertisementData: [
                                        CBAdvertisementDataLocalNameKey: "",
                                        CBAdvertisementDataServiceUUIDsKey: [CBUUID(string: "4553")]
                                       ], rssi: -50)
        try expect(fixture.manager.devices[selectedID] == "ESTKme-RED", "empty later name erased known reader identity")
        switch fixture.manager.protocols[selectedID] {
        case .some(.ccid): break
        default: throw CheckFailure(description: "empty later name downgraded the selected reader protocol")
        }
        try expect(BLETestDiagnostics.lines.isEmpty, "empty later name caused a duplicate diagnostic record")

        fixture.manager.centralManager(fixture.central, didDiscover: fixture.reader.peripheral,
                                       advertisementData: [CBAdvertisementDataLocalNameKey: "Unrelated Name"], rssi: -50)
        try expect(fixture.manager.devices[selectedID] == "ESTKme-RED", "unrelated later name replaced a recognized reader")
        switch fixture.manager.protocols[selectedID] {
        case .some(.ccid): break
        default: throw CheckFailure(description: "unrelated later name downgraded a recognized reader protocol")
        }
        try expect(BLETestDiagnostics.lines.isEmpty, "unrelated later name caused a diagnostic record")

        let newcomer = Reader(name: "BeeSIM")
        fixture.discover(newcomer)
        try expect(fixture.manager.devices[newcomer.peripheral.identifier] == "BeeSIM", "new reader was not discovered")
        try expect(BLETestDiagnostics.lines.count == 1, "new reader did not create exactly one diagnostic record")

        fixture.manager.centralManager(fixture.central, didDiscover: newcomer.peripheral,
                                       advertisementData: [CBAdvertisementDataLocalNameKey: "BeeSIM 2"], rssi: -50)
        try expect(fixture.manager.devices[newcomer.peripheral.identifier] == "BeeSIM 2", "renamed reader was not refreshed")
        try expect(BLETestDiagnostics.lines.count == 2, "reader name change did not create one diagnostic record")

        fixture.discover(newcomer)
        try expect(fixture.manager.devices[newcomer.peripheral.identifier] == "BeeSIM 2", "name-less advertisement reverted a known reader name")
        try expect(BLETestDiagnostics.lines.count == 2, "name-less advertisement caused a duplicate diagnostic record")

        let serviceOnly = Reader(name: "")
        fixture.manager.centralManager(fixture.central, didDiscover: serviceOnly.peripheral,
                                       advertisementData: [CBAdvertisementDataServiceUUIDsKey: [CBUUID(string: "4553")]], rssi: -50)
        try expect(fixture.manager.devices[serviceOnly.peripheral.identifier] == "eSIM Reader", "service-only reader was not discovered")
        try expect(BLETestDiagnostics.lines.count == 3, "service-only reader did not create one diagnostic record")
        fixture.manager.centralManager(fixture.central, didDiscover: serviceOnly.peripheral,
                                       advertisementData: [CBAdvertisementDataLocalNameKey: "ESTKme-RED"], rssi: -50)
        try expect(fixture.manager.devices[serviceOnly.peripheral.identifier] == "ESTKme-RED", "late local name was not applied")
        switch fixture.manager.protocols[serviceOnly.peripheral.identifier] {
        case .some(.ccid): break
        default: throw CheckFailure(description: "late local name did not select CCID framing")
        }
        try expect(BLETestDiagnostics.lines.count == 4, "late local name did not create exactly one diagnostic record")
    }

    static func cachedNameRefreshesWithoutAdvertisedLocalName() throws {
        BLETestQueue.main = BLETestQueue()
        BLETestDiagnostics.reset()
        let manager = BluetoothReaderManager()
        let central = CBCentralManager(delegate: manager, queue: BLETestQueue.main)
        manager.central = central
        let reader = Reader(name: "SIMLink")
        let id = reader.peripheral.identifier
        let estkService = [CBAdvertisementDataServiceUUIDsKey: [CBUUID(string: "4553")]]
        manager.centralManager(central, didDiscover: reader.peripheral,
                               advertisementData: estkService, rssi: -50)
        try expect(manager.devices[id] == "SIMLink", "initial cached reader name was not selected")
        switch manager.protocols[id] {
        case .some(.legacy): break
        default: throw CheckFailure(description: "initial cached name did not select legacy framing")
        }

        // The peripheral cache can be corrected after the first service-only
        // packet; no advertised LocalName was ever seen for this UUID.
        reader.peripheral.name = "ESTKme-RED"
        manager.centralManager(central, didDiscover: reader.peripheral,
                               advertisementData: estkService, rssi: -50)
        try expect(manager.devices[id] == "ESTKme-RED", "updated cached name remained hidden by the old display name")
        switch manager.protocols[id] {
        case .some(.ccid): break
        default: throw CheckFailure(description: "updated cached ESTK name did not select CCID framing")
        }

        // After an explicit local name arrives, a later nameless packet must
        // not undo it just because peripheral.name lags behind.
        manager.centralManager(central, didDiscover: reader.peripheral,
                               advertisementData: [CBAdvertisementDataLocalNameKey: "BeeSIM",
                                                   CBAdvertisementDataServiceUUIDsKey: [CBUUID(string: "4553")]], rssi: -50)
        reader.peripheral.name = "ESTKme-RED"
        manager.centralManager(central, didDiscover: reader.peripheral,
                               advertisementData: estkService, rssi: -50)
        try expect(manager.devices[id] == "BeeSIM", "nameless packet undid an explicit advertised name")
        switch manager.protocols[id] {
        case .some(.beeSim): break
        default: throw CheckFailure(description: "nameless packet undid advertised BeeSIM framing")
        }
    }

    static func activeRescanKeepsConnectedReaderIdentityAndFraming() throws {
        let fixture = Fixture()
        fixture.beginHandshake()
        let manager = fixture.manager
        let reader = fixture.reader
        let id = reader.peripheral.identifier
        fixture.central.connectedPeripherals = [reader.peripheral]

        // The claim APDU is already in flight, but connectedID is only
        // published after power-on. A scan callback must not switch the
        // response parser to another framing protocol halfway through it.
        manager.scan()
        manager.centralManager(fixture.central, didDiscover: reader.peripheral,
                               advertisementData: [CBAdvertisementDataLocalNameKey: "SIMLink",
                                                   CBAdvertisementDataServiceUUIDsKey: [CBUUID(string: "4553")]], rssi: -50)
        switch manager.protocols[id] {
        case .some(.ccid): break
        default: throw CheckFailure(description: "scan changed framing during an in-flight claim APDU")
        }
        try fixture.finishHandshake()

        // A system-retrieved peripheral can have a nil or stale cache name.
        // Scanning while this reader carries a session must not replace the
        // selected CCID identity with placeholder/legacy framing.
        reader.peripheral.name = nil
        manager.scan()
        try expect(manager.connectedID == id && manager.state == .connected,
                   "rescan interrupted the connected reader")
        try expect(manager.devices[id] == "ESTKme-RED", "nil retrieved name replaced the connected reader")
        switch manager.protocols[id] {
        case .some(.ccid): break
        default: throw CheckFailure(description: "nil retrieved name downgraded the active CCID protocol")
        }

        reader.peripheral.name = "SIMLink"
        manager.scan()
        try expect(manager.devices[id] == "ESTKme-RED", "stale retrieved name replaced the connected reader")
        switch manager.protocols[id] {
        case .some(.ccid): break
        default: throw CheckFailure(description: "stale retrieved name downgraded the active CCID protocol")
        }

        // An unrelated system-connected reader may also have an explicit
        // advertised name newer than CoreBluetooth's cache. Re-scanning the
        // active reader must not make that other entry sticky-stale either.
        let other = Reader(name: "BeeSIM")
        let otherID = other.peripheral.identifier
        manager.centralManager(fixture.central, didDiscover: other.peripheral,
                               advertisementData: [CBAdvertisementDataLocalNameKey: "BeeSIM"], rssi: -50)
        other.peripheral.name = "SIMLink"
        fixture.central.connectedPeripherals = [reader.peripheral, other.peripheral]
        manager.scan()
        try expect(manager.devices[otherID] == "BeeSIM", "retrieved stale cache replaced an advertised name")
        switch manager.protocols[otherID] {
        case .some(.beeSim): break
        default: throw CheckFailure(description: "retrieved stale cache changed another reader's framing")
        }

        manager.centralManager(fixture.central, didDiscover: reader.peripheral,
                               advertisementData: [CBAdvertisementDataLocalNameKey: "SIMLink",
                                                   CBAdvertisementDataServiceUUIDsKey: [CBUUID(string: "4553")]], rssi: -50)
        try expect(manager.devices[id] == "ESTKme-RED", "advertisement changed an active reader identity")
        switch manager.protocols[id] {
        case .some(.ccid): break
        default: throw CheckFailure(description: "advertisement changed active APDU framing")
        }

        let earlierWrites = reader.peripheral.writes.count
        manager.exchangeAPDU(Data([0x00, 0xa4, 0x00, 0x00])) { _ in }
        try expect(reader.peripheral.writes.count == earlierWrites + 1,
                   "active rescan blocked the next APDU")
        try expect(reader.peripheral.writes[earlierWrites].first == 0x6f,
                   "next APDU after rescan did not use CCID framing")
        fixture.reply()
        try expect(manager.pendingReply == nil, "post-rescan APDU reply was not completed")
        try expect(fixture.central.cancellations.isEmpty, "rescan cancelled a connected reader")
    }

    static func discoveryRefreshesReconnectReferenceWithoutSwitchingLiveTransport() throws {
        for duringHandshake in [true, false] {
            for source in 0..<3 { // retrieval only, advertisement only, both
                let fixture = Fixture()
                fixture.beginHandshake()
                if !duringHandshake { try fixture.finishHandshake() }
                let manager = fixture.manager
                let original = fixture.reader.peripheral
                let id = original.identifier

                let viaSystem = CBPeripheral(name: "SIMLink", identifier: id)
                if source != 1 {
                    fixture.central.connectedPeripherals = [viaSystem]
                    manager.scan()
                    try expect(manager.peripherals[id] === original,
                               "system retrieval switched the live APDU transport")
                }

                let viaAdvertisement = CBPeripheral(name: "ESTKme-RED", identifier: id)
                if source != 0 {
                    manager.centralManager(fixture.central, didDiscover: viaAdvertisement,
                                           advertisementData: [CBAdvertisementDataLocalNameKey: "ESTKme-RED"], rssi: -50)
                    try expect(manager.peripherals[id] === original,
                               "advertisement switched the live APDU transport")
                }
                switch manager.protocols[id] {
                case .some(.ccid): break
                default: throw CheckFailure(description: "discovery changed live APDU framing")
                }

                if duringHandshake { try fixture.finishHandshake() }
                else {
                    let earlierWrites = original.writes.count
                    manager.exchangeAPDU(Data([0x00, 0xa4, 0x00, 0x00])) { _ in }
                    try expect(original.writes.count == earlierWrites + 1,
                               "post-discovery APDU was not written through original peripheral")
                    try expect(viaSystem.writes.isEmpty && viaAdvertisement.writes.isEmpty,
                               "post-discovery APDU was written through a replacement peripheral")
                    fixture.reply()
                }

                let newest = source == 0 ? viaSystem : viaAdvertisement
                fixture.disconnected()
                fixture.clock.advance(1)
                try expect(manager.peripherals[id] === newest,
                           "retry did not adopt the newest discovered peripheral")
                try expect(fixture.central.connectionObjects.last === newest,
                           "retry still used the stale peripheral object")
                try expect(manager.devices[id] == "ESTKme-RED",
                           "stale retrieved name changed the next attempt's reader identity")
                switch manager.protocols[id] {
                case .some(.ccid): break
                default: throw CheckFailure(description: "stale retrieved name changed the next attempt's CCID framing")
                }
            }
        }
    }

    static func staleSameIDPeripheralCallbacksCannotDisruptReplacement() throws {
        let fixture = Fixture()
        fixture.beginHandshake()
        try fixture.finishHandshake()
        let manager = fixture.manager
        let original = fixture.reader.peripheral
        let id = original.identifier
        let replacement = Reader(peripheral: CBPeripheral(name: "ESTKme-RED", identifier: id))

        manager.centralManager(fixture.central, didDiscover: replacement.peripheral,
                               advertisementData: [CBAdvertisementDataLocalNameKey: "ESTKme-RED"], rssi: -50)
        fixture.disconnected()
        fixture.clock.advance(1)
        try expect(manager.peripherals[id] === replacement.peripheral,
                   "retry did not select the replacement peripheral object")
        try expect(manager.state == .connecting, "replacement retry did not start")

        // UUIDs are equal, but these callbacks belong to the previous GATT
        // object. They must not complete or fail the replacement attempt.
        original.state = .connected
        let priorCancellations = fixture.central.cancellationObjects.count
        manager.centralManager(fixture.central, didConnect: original)
        try expect(fixture.central.cancellationObjects.dropFirst(priorCancellations).contains { $0 === original },
                   "late old connection was not retired")
        try expect(!fixture.central.cancellationObjects.dropFirst(priorCancellations).contains { $0 === replacement.peripheral },
                   "retiring the old object cancelled the replacement")
        manager.centralManager(fixture.central, didFailToConnect: original, error: nil)
        try expect(manager.state == .connecting,
                   "old connect/failure callback changed the replacement attempt")

        fixture.connected(replacement)
        original.state = .connected
        manager.peripheral(original, didDiscoverServices: nil)
        manager.peripheral(original, didDiscoverCharacteristicsFor: fixture.reader.service, error: nil)
        try expect(manager.notifyCharacteristic == nil && manager.writeCharacteristic == nil,
                   "old service callback selected characteristics on the replacement link")

        fixture.services(replacement)
        fixture.characteristics(replacement)
        fixture.notificationReady(replacement)
        try fixture.finishHandshake(replacement)
        try expect(manager.connectedID == id && manager.state == .connected,
                   "replacement handshake did not complete")

        let connectedCancellations = fixture.central.cancellationObjects.count
        original.state = .connected
        original.delegate = manager
        manager.centralManager(fixture.central, didConnect: original)
        try expect(manager.connectedID == id && manager.state == .connected,
                   "late old connect callback interrupted the established replacement")
        try expect(fixture.central.cancellationObjects.count == connectedCancellations,
                   "retiring old object cancelled an already connected replacement")
        try expect(original.delegate == nil, "late old peripheral retained its delegate")

        manager.centralManager(fixture.central, didDisconnectPeripheral: original, error: nil)
        try expect(manager.connectedID == id && manager.state == .connected,
                   "old disconnect callback revoked the replacement link")

        // With the current peripheral flow-controlled, a late readiness
        // callback from the old object must not drain its write queue.
        replacement.peripheral.canSendWriteWithoutResponse = false
        let replacementWrites = replacement.peripheral.writes.count
        let originalWrites = original.writes.count
        manager.exchangeRaw(Data([0x6f, 0x00])) { _ in }
        try expect(manager.pendingReply != nil, "replacement APDU did not start")
        manager.peripheralIsReady(toSendWriteWithoutResponse: original)
        try expect(original.writes.count == originalWrites,
                   "old peripheral drained the replacement APDU write queue")
        replacement.peripheral.canSendWriteWithoutResponse = true
        manager.peripheralIsReady(toSendWriteWithoutResponse: replacement.peripheral)
        try expect(replacement.peripheral.writes.count == replacementWrites + 1,
                   "replacement did not resume its own APDU write queue")
    }

    static func protocolAndObjectArePinnedThroughCurrentHandshake() throws {
        let fixture = Fixture()
        let manager = fixture.manager
        let original = fixture.reader.peripheral
        let id = original.identifier
        let next = Reader(name: "SIMLink", peripheral: CBPeripheral(name: "SIMLink", identifier: id))

        // The new advertisement arrives after connect() but before GATT and
        // claim. Its legacy framing applies only to the next link attempt.
        manager.centralManager(fixture.central, didDiscover: next.peripheral,
                               advertisementData: [CBAdvertisementDataLocalNameKey: "SIMLink",
                                                   CBAdvertisementDataServiceUUIDsKey: [CBUUID(string: "4553")]], rssi: -50)
        try expect(manager.peripherals[id] === original,
                   "advertisement switched the in-flight GATT object")
        try expect(manager.devices[id] == "ESTKme-RED", "advertisement changed the active reader name")
        switch manager.protocols[id] {
        case .some(.ccid): break
        default: throw CheckFailure(description: "advertisement changed in-flight CCID framing")
        }
        fixture.beginHandshake()
        try fixture.finishHandshake()
        let priorWrites = original.writes.count
        manager.exchangeAPDU(Data([0x00, 0xa4, 0x00, 0x00])) { _ in }
        try expect(original.writes.count == priorWrites + 1 && original.writes[priorWrites].first == 0x6f,
                   "active APDU did not keep original CCID framing")
        fixture.reply()

        fixture.disconnected()
        fixture.clock.advance(1)
        try expect(manager.peripherals[id] === next.peripheral,
                   "next attempt did not adopt staged advertised object")
        try expect(manager.devices[id] == "SIMLink", "next attempt did not adopt staged name")
        switch manager.protocols[id] {
        case .some(.legacy): break
        default: throw CheckFailure(description: "next attempt did not adopt staged legacy framing")
        }
        fixture.beginHandshake(next)
        try fixture.finishHandshake(next)
        try expect(manager.connectedID == id && manager.state == .connected,
                   "staged protocol could not complete its own handshake")
    }

    static func advertisedNextAttemptSurvivesStaleSystemRetrieval() throws {
        for advertisementFirst in [false, true] {
            let fixture = Fixture()
            fixture.beginHandshake()
            try fixture.finishHandshake()
            let manager = fixture.manager
            let id = fixture.reader.peripheral.identifier
            let advertised = Reader(name: "BeeSIM", peripheral: CBPeripheral(name: "BeeSIM", identifier: id))
            let retrieved = CBPeripheral(name: "SIMLink", identifier: id)
            fixture.central.connectedPeripherals = [retrieved]
            let deliverAdvertisement = {
                manager.centralManager(fixture.central, didDiscover: advertised.peripheral,
                                       advertisementData: [CBAdvertisementDataLocalNameKey: "BeeSIM",
                                                           CBAdvertisementDataServiceUUIDsKey: [CBUUID(string: "AE30")]], rssi: -50)
            }
            if advertisementFirst { deliverAdvertisement() }
            manager.scan()
            if !advertisementFirst { deliverAdvertisement() }

            try expect(manager.peripherals[id] === fixture.reader.peripheral,
                       "rescan replaced the active APDU object")
            try expect(manager.devices[id] == "ESTKme-RED",
                       "rescan changed the active reader identity")
            switch manager.protocols[id] {
            case .some(.ccid): break
            default: throw CheckFailure(description: "rescan changed the active CCID protocol")
            }
            fixture.disconnected()
            fixture.clock.advance(1)
            try expect(manager.peripherals[id] === advertised.peripheral,
                       "stale retrieveConnected replaced the staged advertisement (advertisementFirst=\(advertisementFirst))")
            try expect(manager.devices[id] == "BeeSIM",
                       "next attempt did not adopt advertised BeeSIM name")
            switch manager.protocols[id] {
            case .some(.beeSim): break
            default: throw CheckFailure(description: "next attempt did not adopt advertised BeeSIM framing")
            }
        }
    }

    static func unrecognizedServiceNameDoesNotDowngradeKnownFraming() throws {
        let fixture = Fixture()
        fixture.beginHandshake()
        try fixture.finishHandshake()
        let manager = fixture.manager
        let id = fixture.reader.peripheral.identifier
        let alternate = CBPeripheral(name: "Unrelated Name", identifier: id)
        manager.centralManager(fixture.central, didDiscover: alternate,
                               advertisementData: [CBAdvertisementDataLocalNameKey: "Unrelated Name",
                                                   CBAdvertisementDataServiceUUIDsKey: [CBUUID(string: "4553")]], rssi: -50)
        fixture.disconnected()
        fixture.clock.advance(1)
        try expect(manager.devices[id] == "ESTKme-RED",
                   "unrecognized service-only name replaced the known reader identity")
        switch manager.protocols[id] {
        case .some(.ccid): break
        default: throw CheckFailure(description: "unrecognized service-only name downgraded CCID framing")
        }
    }

    static func unselectedReaderAdvertisementBeatsStaleRetrieval() throws {
        let fixture = Fixture()
        fixture.beginHandshake()
        try fixture.finishHandshake()
        let manager = fixture.manager
        let newReader = Reader(name: "BeeSIM")
        let id = newReader.peripheral.identifier
        let staleSystemObject = CBPeripheral(name: "SIMLink", identifier: id)

        manager.centralManager(fixture.central, didDiscover: newReader.peripheral,
                               advertisementData: [CBAdvertisementDataLocalNameKey: "BeeSIM",
                                                   CBAdvertisementDataServiceUUIDsKey: [CBUUID(string: "AE30")]], rssi: -50)
        fixture.central.connectedPeripherals = [staleSystemObject]
        manager.scan()
        try expect(manager.peripherals[id] === newReader.peripheral,
                   "stale system retrieval replaced an unselected advertised object")
        try expect(manager.devices[id] == "BeeSIM", "stale retrieval replaced the advertised name")
        switch manager.protocols[id] {
        case .some(.beeSim): break
        default: throw CheckFailure(description: "stale retrieval replaced advertised BeeSIM framing")
        }

        manager.connect(id)
        try expect(fixture.central.connectionObjects.last === newReader.peripheral,
                   "connect bound stale retrieved object instead of newer advertisement")
        fixture.beginHandshake(newReader)
        try fixture.finishHandshake(newReader)
        try expect(manager.connectedID == id && manager.state == .connected,
                   "advertised second reader could not complete its handshake")

        // A manual disconnect followed by a fresh scan resets provenance, so
        // the next scan can discover a different system-connected object.
        manager.disconnect()
        manager.scan()
        try expect(manager.peripherals[id] === staleSystemObject,
                   "fresh discovery did not reset advertised-object provenance")
    }

    static func beeSimFrameCountBoundaryDoesNotTrap() throws {
        let fixture = Fixture(name: "BeeSIM")
        fixture.beginHandshake()
        try fixture.finishHandshake()
        let manager = fixture.manager
        let peripheral = fixture.reader.peripheral
        let payloadSize = 242 // The test adapter advertises a 244-byte write.
        let largest = Data(repeating: 0xa5, count: payloadSize * 255)
        guard let encoded = manager.encodeBeeSimFrames(largest, peripheral: peripheral) as Data? else {
            throw CheckFailure(description: "255 BeeSIM frames were rejected")
        }
        try expect(encoded.count == 244 * 255, "valid BeeSIM encoding length changed")
        for index in 0..<255 {
            try expect(encoded[index * 244] == 255 && encoded[index * 244 + 1] == UInt8(index + 1),
                       "valid BeeSIM frame count/index changed")
            try expect(encoded.subdata(in: (index * 244 + 2)..<((index + 1) * 244))
                        == Data(repeating: 0xa5, count: payloadSize), "valid BeeSIM payload changed")
        }
        try expect(manager.encodeBeeSimFrames(Data(), peripheral: peripheral) == Data([1, 1]),
                   "empty BeeSIM command changed")

        let writesBefore = peripheral.writes.count
        var results: [BluetoothReaderErrorCode] = []
        manager.exchangeRaw(Data(repeating: 0xa5, count: payloadSize * 255 + 1)) { result in
            if case .failure(let error as BluetoothReaderTransportError) = result { results.append(error.code) }
        }
        try expect(results == [.apduMalformed], "256-frame exchange did not fail exactly once")
        try expect(peripheral.writes.count == writesBefore && manager.pendingReply == nil && manager.replyTimeout == nil,
                   "rejected exchange wrote data or retained its callback/deadline")
        fixture.clock.advance(121)
        try expect(manager.state == .connected && manager.connectedID == peripheral.identifier,
                   "oversized local request tore down a healthy connection")

        var completed = false
        manager.exchangeRaw(Data([0, 0xa4, 4, 0])) { result in
            if case .success = result { completed = true }
        }
        fixture.reply()
        try expect(completed, "valid request could not run after rejected oversized request")
    }

    static func main() {
        let iterationsText = ProcessInfo.processInfo.environment["BLE_MANAGER_ITERATIONS"] ?? "1"
        guard let iterations = Int(iterationsText), (1...10_000).contains(iterations) else {
            print("BLE_MANAGER_ITERATIONS must be an integer from 1 to 10000")
            exit(2)
        }
        let tests: [(String, () throws -> Void)] = [
            ("central initialization timeout permits fresh retry", initialCentralTimeoutAllowsRetry),
            ("initial state callback cancels central timeout", initialCentralCallbackCancelsTimeout),
            ("closing pending initialization cancels timeout", closingDuringCentralInitializationCancelsTimeout),
            ("permission prompt survives central initialization deadline", permissionPromptOutlivesInitializationDeadline),
            ("grant without state callback permits bounded retry", grantWithoutStateCallbackStillTimesOut),
            ("sheet close keeps late permission grant for next scan", sheetCloseDuringPermissionPromptKeepsGrantForNextScan),
            ("denial blocks queued retry and preserves authorized recovery", denialBlocksQueuedReconnectAndAllowsLaterRecovery),
            ("publish connected ID only after APDU ready", readyBeforeConnectedIDPublication),
            ("late services/errors and duplicate notify, all protocols", lateServicesAndDuplicateNotify),
            ("GATT writer capability selection and with-response pacing", characteristicWriteModesAndResponsePacing),
            ("connected notification loss/error and callback-free recovery", notificationLoss),
            ("superseded reader callbacks and deadlines", supersededReaderCallbacks),
            ("four phase timeouts and delayed disconnect", phaseTimeoutsAndDelayedDisconnect),
            ("manual disconnect cancels phase/APDU deadlines", manualDisconnectCancelsDeadlines),
            ("120s APDU deadline and stale response isolation", apduTimeoutIsolatedFromGATTDeadline),
            ("revoke transport before optional claim continuation", channelRevokedBeforeContinuation),
            ("duplicate discovery is bounded without losing readers", discoveryDeduplicatesDiagnosticsWithoutLosingReaders),
            ("cached reader name updates without advertised LocalName", cachedNameRefreshesWithoutAdvertisedLocalName),
            ("active rescan preserves reader identity and framing", activeRescanKeepsConnectedReaderIdentityAndFraming),
            ("live transport stays fixed while reconnect reference refreshes", discoveryRefreshesReconnectReferenceWithoutSwitchingLiveTransport),
            ("stale same-ID callbacks cannot disrupt replacement", staleSameIDPeripheralCallbacksCannotDisruptReplacement),
            ("attempt pins object and framing until next handshake", protocolAndObjectArePinnedThroughCurrentHandshake),
            ("staged advertisement beats stale retrieval in both orders", advertisedNextAttemptSurvivesStaleSystemRetrieval),
            ("unrecognized service-only name keeps known framing", unrecognizedServiceNameDoesNotDowngradeKnownFraming),
            ("unselected reader advertisement beats stale retrieval", unselectedReaderAdvertisementBeatsStaleRetrieval),
            ("BeeSIM 255/256 frame boundary and healthy-link recovery", beeSimFrameCountBoundaryDoesNotTrap)
        ]
        let filter = ProcessInfo.processInfo.environment["BLE_MANAGER_TEST_FILTER"]
        let selectedTests = tests.filter { filter == nil || $0.0.contains(filter!) }
        guard !selectedTests.isEmpty else { print("No BLE manager tests matched filter"); exit(2) }
        var failures = 0
        for (name, test) in selectedTests {
            do {
                for iteration in 1...iterations {
                    do { try test() }
                    catch { throw CheckFailure(description: "iteration \(iteration): \(error)") }
                }
                print("PASS: \(name) (\(iterations) iteration(s))")
            }
            catch { failures += 1; print("FAIL: \(name): \(error)") }
        }
        if failures != 0 { exit(1) }
        print("BLE manager: \(selectedTests.count) production event-sequence groups × \(iterations) passed (virtual time; no hardware)")
    }
}
