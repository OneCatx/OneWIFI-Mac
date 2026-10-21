import AVFoundation
import AudioToolbox
import CoreAudio
import Foundation
import Network

/// Real-time PCM bridge for reader VoWiFi calls. Carrier RTP remains in the
/// backend SWu netstack; this loopback-only channel carries microphone/speaker
/// samples and cannot route IMS traffic around the configured proxy.
final class ReaderVoWiFiAudioBridge: @unchecked Sendable {
    static let shared = ReaderVoWiFiAudioBridge()

    private let queue = DispatchQueue(label: "com.proletarius.reader-audio", qos: .userInteractive)
    private let queueKey = DispatchSpecificKey<Void>()
    private let engine = AVAudioEngine()
    private let player = AVAudioPlayerNode()
    private let ringbackPlayer = AVAudioPlayerNode()
    private var connection: NWConnection?
    private var token = Data()
    private var microphoneTapGeneration: UInt64 = 0
    private var playbackConverter: AVAudioConverter?
    private var playbackInputFormat: AVAudioFormat?
    private var playbackFormat: AVAudioFormat?
    private var running = false
    private var tapInstalled = false
    private var ringbackRunning = false
    private var pendingPlaybackBuffers: [AVAudioPCMBuffer] = []
    private var queuedPlaybackBufferCount = 0
    private var playbackPrimed = false
    private var playbackGeneration: UInt64 = 0
    private var playbackPreviousInput: Float = 0
    private var playbackPreviousOutput: Float = 0
    private var playbackLastSample: Float = 0
    private var playbackFadeSamplesRemaining = 40
    private var microphonePCM: [Int16] = []
    private let microphoneQueueSlots = DispatchSemaphore(value: 8)
    private var microphonePreviousInput: Float = 0
    private var microphonePreviousOutput: Float = 0
    private var microphoneNoiseFloor: Float = 0.003
    private var microphoneGateGain: Float = 1
    private var microphoneGateOpen = true
    private var microphoneGateHoldFrames = 0
    private var microphoneRequested = false
    private var recoveringAudioRoute = false
    private var routeRecoveryAttempts = 0
    private var lastAudioRouteError: String?
    private var systemVoiceProcessingActive = false
    private var configurationObserver: NSObjectProtocol?
    private var sessionGeneration: UInt64 = 0

    private init() {
        queue.setSpecific(key: queueKey, value: ())
        configurationObserver = NotificationCenter.default.addObserver(
            forName: .AVAudioEngineConfigurationChange,
            object: engine,
            queue: nil
        ) { [weak self] _ in
            self?.queue.async { self?.recoverFromAudioRouteChange() }
        }
    }

    func start(
        port: Int,
        token: String,
        captureMicrophone: Bool,
        localRingback: Bool
    ) async -> String? {
        stop()
        let generation = syncOnAudioQueue { sessionGeneration }
        guard port > 0, let nwPort = NWEndpoint.Port(rawValue: UInt16(port)) else {
            return "读卡器音频桥端口无效。"
        }
        self.token = Data(token.utf8) + Data([0])
        engine.attach(player)
        engine.attach(ringbackPlayer)
        // Keep the player-to-mixer format stable while macOS converts the
        // mixer output to the current hardware route. AirPods can switch from
        // 48 kHz stereo A2DP to a lower-rate mono hands-free profile as soon
        // as its microphone opens; binding buffers to the transient hardware
        // format would otherwise cause noise, silence or a format crash.
        guard let playbackFormat = AVAudioFormat(
            standardFormatWithSampleRate: 48_000,
            channels: 2) else {
            stop()
            return "无法创建 Mac 通话混音格式。"
        }
        self.playbackFormat = playbackFormat
        guard let playbackInputFormat = AVAudioFormat(
                commonFormat: .pcmFormatFloat32,
                sampleRate: 8_000,
                channels: 1,
                interleaved: false),
              let playbackConverter = AVAudioConverter(
                from: playbackInputFormat,
                to: playbackFormat) else {
            stop()
            return "无法创建高质量通话播放转换器。"
        }
        playbackConverter.primeMethod = .none
        playbackConverter.sampleRateConverterQuality = Int(kAudioConverterQuality_Max)
        self.playbackInputFormat = playbackInputFormat
        self.playbackConverter = playbackConverter
        // AVAudioPlayerNode requires every scheduled buffer to match the
        // format of its output connection exactly. Connecting with nil and
        // then scheduling 8 kHz mono buffers causes an uncaught NSException
        // on common 48 kHz stereo Mac outputs.
        engine.connect(player, to: engine.mainMixerNode, format: playbackFormat)
        engine.connect(ringbackPlayer, to: engine.mainMixerNode, format: playbackFormat)
        let connection = NWConnection(host: "127.0.0.1", port: nwPort, using: .udp)
        self.connection = connection
        connection.stateUpdateHandler = { _ in }
        connection.start(queue: queue)
        receiveNext(connection)

        if captureMicrophone, let error = await enableMicrophone(expectedGeneration: generation) {
            stop(ifCurrentGeneration: generation)
            return error
        }
        let startError: String? = syncOnAudioQueue {
            guard sessionGeneration == generation else { return "读卡器通话音频启动已取消。" }
            do {
                try engine.start()
                running = true
                setLocalRingbackEnabledOnQueue(localRingback)
                return nil
            } catch {
                return "启动读卡器通话音频失败：\(error.localizedDescription)"
            }
        }
        if let startError {
            stop(ifCurrentGeneration: generation)
            return startError
        }
        let isCurrentSession = syncOnAudioQueue { sessionGeneration == generation }
        guard isCurrentSession else {
            return "读卡器通话音频启动已取消。"
        }
        connection.send(content: self.token, completion: .contentProcessed { _ in })
        return nil
    }

    func enableMicrophone() async -> String? {
        let generation = syncOnAudioQueue { sessionGeneration }
        return await enableMicrophone(expectedGeneration: generation)
    }

    private func enableMicrophone(expectedGeneration: UInt64) async -> String? {
        let granted = await requestMicrophoneAccess()
        guard granted else { return "请在系统设置中允许 OneWIFI 使用麦克风。" }
        return syncOnAudioQueue {
            guard sessionGeneration == expectedGeneration, connection != nil else {
                return "读卡器通话音频会话已结束。"
            }
            microphoneRequested = true
            if tapInstalled { return nil }
            let wasRunning = engine.isRunning
            if wasRunning { engine.pause() }
            let error = installMicrophoneTapOnQueue()
            if wasRunning {
                do {
                    try engine.start()
                    resumePlayersAfterEngineStart()
                } catch {
                    return "切换麦克风后无法恢复通话音频：\(error.localizedDescription)"
                }
            }
            return error
        }
    }

    private func installMicrophoneTapOnQueue() -> String? {
        let input = engine.inputNode
        // Best-effort system voice processing provides acoustic echo
        // cancellation and hardware-aware noise suppression. Some aggregate
        // audio devices do not support it, so failure must not block a call.
        do {
            try input.setVoiceProcessingEnabled(true)
            systemVoiceProcessingActive = input.isVoiceProcessingEnabled
        } catch {
            // AirPods, aggregate devices and some external interfaces may not
            // expose the macOS voice-processing unit. The PCM conditioner below
            // remains active, so the call continues with a deterministic
            // software fallback instead of failing audio setup.
            systemVoiceProcessingActive = false
        }
        let inputFormat = input.outputFormat(forBus: 0)
        guard inputFormat.sampleRate > 0,
              inputFormat.channelCount > 0,
              let sendFormat = AVAudioFormat(
                commonFormat: .pcmFormatInt16,
                sampleRate: 8_000,
                channels: 1,
                interleaved: true),
              let converter = AVAudioConverter(from: inputFormat, to: sendFormat) else {
            return "无法连接当前 Mac 麦克风或创建 8 kHz 通话转换器。"
        }
        microphoneTapGeneration &+= 1
        let tapGeneration = microphoneTapGeneration
        input.installTap(onBus: 0, bufferSize: 960, format: inputFormat) { [weak self] buffer, _ in
            self?.convertMicrophone(
                buffer, format: sendFormat, converter: converter,
                tapGeneration: tapGeneration)
        }
        tapInstalled = true
        return nil
    }

    func setLocalRingbackEnabled(_ enabled: Bool) {
        syncOnAudioQueue {
            setLocalRingbackEnabledOnQueue(enabled)
        }
    }

    private func setLocalRingbackEnabledOnQueue(_ enabled: Bool) {
        guard enabled else {
            if ringbackRunning {
                ringbackPlayer.stop()
                ringbackRunning = false
            }
            return
        }
        guard !ringbackRunning,
              let format = playbackFormat,
              let buffer = makeRingbackBuffer(format: format) else { return }
        ringbackPlayer.scheduleBuffer(buffer, at: nil, options: .loops)
        ringbackPlayer.play()
        ringbackRunning = true
    }

    func audioRoute() -> (input: String, output: String) {
        (defaultDeviceName(selector: kAudioHardwarePropertyDefaultInputDevice),
         defaultDeviceName(selector: kAudioHardwarePropertyDefaultOutputDevice))
    }

    func audioRouteError() -> String? {
        syncOnAudioQueue { lastAudioRouteError }
    }

    func audioProcessingDescription() -> String {
        syncOnAudioQueue {
            systemVoiceProcessingActive
                ? "macOS 回声消除与降噪"
                : "软件抗干扰与限幅"
        }
    }

    func stop() {
        syncOnAudioQueue {
            stopOnAudioQueue()
        }
    }

    private func stop(ifCurrentGeneration generation: UInt64) {
        syncOnAudioQueue {
            guard sessionGeneration == generation else { return }
            stopOnAudioQueue()
        }
    }

    private func stopOnAudioQueue() {
        sessionGeneration &+= 1
        if tapInstalled {
            engine.inputNode.removeTap(onBus: 0)
            tapInstalled = false
        }
        microphoneTapGeneration &+= 1
        player.stop()
        ringbackPlayer.stop()
        ringbackRunning = false
        engine.stop()
        if player.engine != nil { engine.detach(player) }
        if ringbackPlayer.engine != nil { engine.detach(ringbackPlayer) }
        connection?.cancel()
        connection = nil
        playbackConverter = nil
        playbackInputFormat = nil
        playbackFormat = nil
        pendingPlaybackBuffers.removeAll(keepingCapacity: true)
        queuedPlaybackBufferCount = 0
        playbackPrimed = false
        playbackGeneration &+= 1
        playbackPreviousInput = 0
        playbackPreviousOutput = 0
        playbackLastSample = 0
        playbackFadeSamplesRemaining = 40
        microphonePCM.removeAll(keepingCapacity: true)
        microphonePreviousInput = 0
        microphonePreviousOutput = 0
        microphoneNoiseFloor = 0.003
        microphoneGateGain = 1
        microphoneGateOpen = true
        microphoneGateHoldFrames = 0
        microphoneRequested = false
        recoveringAudioRoute = false
        routeRecoveryAttempts = 0
        lastAudioRouteError = nil
        systemVoiceProcessingActive = false
        token = Data()
        running = false
    }

    private func requestMicrophoneAccess() async -> Bool {
        switch AVCaptureDevice.authorizationStatus(for: .audio) {
        case .authorized:
            return true
        case .notDetermined:
            return await withCheckedContinuation { continuation in
                AVCaptureDevice.requestAccess(for: .audio) { granted in
                    continuation.resume(returning: granted)
                }
            }
        case .denied, .restricted:
            return false
        @unknown default:
            return false
        }
    }

    private func convertMicrophone(
        _ input: AVAudioPCMBuffer,
        format: AVAudioFormat,
        converter: AVAudioConverter,
        tapGeneration: UInt64
    ) {
        let ratio = format.sampleRate / input.format.sampleRate
        let capacity = max(160, AVAudioFrameCount(Double(input.frameLength) * ratio) + 16)
        guard let output = AVAudioPCMBuffer(pcmFormat: format, frameCapacity: capacity) else { return }
        var supplied = false
        var conversionError: NSError?
        let status = converter.convert(to: output, error: &conversionError) { _, state in
            if supplied {
                state.pointee = .noDataNow
                return nil
            }
            supplied = true
            state.pointee = .haveData
            return input
        }
        guard status != .error, output.frameLength > 0,
              let samples = output.int16ChannelData?[0] else { return }
        let count = Int(output.frameLength)
        // The input/output AVAudioPCMBuffer is owned by the tap callback and
        // may be reused immediately. Copy only the converted samples before
        // crossing to the serial route/playback queue.
        let copiedSamples = Array(UnsafeBufferPointer(start: samples, count: count))
        // A stalled output route must not queue seconds of stale microphone
        // audio or grow memory without bound. A dropped 20 ms frame is safer
        // than replaying it after the caller has resumed speaking.
        guard microphoneQueueSlots.wait(timeout: .now()) == .success else { return }
        let slots = microphoneQueueSlots
        queue.async { [weak self] in
            defer { slots.signal() }
            guard let self,
                  self.microphoneTapGeneration == tapGeneration,
                  self.running, self.microphoneRequested,
                  self.connection != nil else { return }
            self.enqueueMicrophoneSamples(copiedSamples)
        }
    }

    private func enqueueMicrophoneSamples(_ samples: [Int16]) {
        guard let connection else { return }
        microphonePCM.append(contentsOf: samples)
        var consumed = 0
        while microphonePCM.count - consumed >= 160 {
            var frame = Array(microphonePCM[consumed..<(consumed + 160)])
            consumed += 160
            conditionMicrophoneFrame(&frame)
            let pcm = frame.withUnsafeBytes { Data($0) }
            connection.send(content: token + pcm, completion: .contentProcessed { _ in })
        }
        if consumed > 0 {
            microphonePCM.removeFirst(consumed)
        }
    }

    private func receiveNext(_ connection: NWConnection) {
        connection.receiveMessage { [weak self] data, _, _, error in
            guard let self, error == nil, self.connection === connection else { return }
            if let data, data.count > self.token.count, data.starts(with: self.token) {
                self.setLocalRingbackEnabledOnQueue(false)
                self.playPCM(data.dropFirst(self.token.count))
            }
            self.receiveNext(connection)
        }
    }

    private func playPCM(_ data: Data.SubSequence) {
        let sourceFrames = data.count / 2
        guard running,
              sourceFrames == 160,
              player.engine != nil,
              let format = playbackFormat,
              let inputFormat = playbackInputFormat,
              let playbackConverter,
              let sourceBuffer = AVAudioPCMBuffer(
                pcmFormat: inputFormat,
                frameCapacity: AVAudioFrameCount(sourceFrames)),
              let sourceSamples = sourceBuffer.floatChannelData?[0] else { return }
        sourceBuffer.frameLength = AVAudioFrameCount(sourceFrames)

        let bytes = Array(data)
        for index in 0..<sourceFrames {
            let low = UInt16(bytes[index * 2])
            let high = UInt16(bytes[index * 2 + 1]) << 8
            let raw = Float(Int16(bitPattern: low | high)) / 32_768
            // Remove DC bias and cap isolated sample jumps caused by a damaged
            // radio/network frame. The limiter is deliberately gentle so it
            // does not flatten normal AMR speech dynamics.
            // Correct one-pole high-pass at roughly 100 Hz. Besides DC, this
            // removes 50/60 Hz mains hum without touching the telephone speech
            // band. Multiplying the input delta by the coefficient is important:
            // omitting it slightly boosts high-frequency codec noise.
            let highPassed = 0.925 * (playbackPreviousOutput + raw - playbackPreviousInput)
            playbackPreviousInput = raw
            playbackPreviousOutput = highPassed
            let limitedDelta = max(-0.55, min(0.55, highPassed - playbackLastSample))
            let conditioned = max(-0.92, min(0.92, playbackLastSample + limitedDelta))
            playbackLastSample = conditioned
            var faded = conditioned
            if playbackFadeSamplesRemaining > 0 {
                let progress = Float(40 - playbackFadeSamplesRemaining + 1) / 40
                faded *= 0.5 - 0.5 * cos(.pi * progress)
                playbackFadeSamplesRemaining -= 1
            }
            sourceSamples[index] = faded
        }
        let expectedFrames = AVAudioFrameCount(
            ceil(Double(sourceFrames) * format.sampleRate / inputFormat.sampleRate) + 32)
        guard let buffer = AVAudioPCMBuffer(pcmFormat: format, frameCapacity: expectedFrames) else { return }
        var supplied = false
        var conversionError: NSError?
        let status = playbackConverter.convert(to: buffer, error: &conversionError) { _, state in
            if supplied {
                state.pointee = .noDataNow
                return nil
            }
            supplied = true
            state.pointee = .haveData
            return sourceBuffer
        }
        guard status != .error, buffer.frameLength > 0 else { return }
        enqueuePlaybackBuffer(buffer)
    }

    private func enqueuePlaybackBuffer(_ buffer: AVAudioPCMBuffer) {
        if !playbackPrimed {
            pendingPlaybackBuffers.append(buffer)
            // Four 20 ms frames absorb Wi-Fi and userspace-netstack scheduling
            // jitter. The added 20 ms is preferable to repeated underruns,
            // which are perceived as electrical crackle.
            guard pendingPlaybackBuffers.count >= 4 else { return }
            playbackPrimed = true
            let primed = pendingPlaybackBuffers
            pendingPlaybackBuffers.removeAll(keepingCapacity: true)
            for item in primed { schedulePlaybackBuffer(item) }
            player.play()
            return
        }
        schedulePlaybackBuffer(buffer)
    }

    private func schedulePlaybackBuffer(_ buffer: AVAudioPCMBuffer) {
        let generation = playbackGeneration
        queuedPlaybackBufferCount += 1
        player.scheduleBuffer(buffer, completionCallbackType: .dataPlayedBack) { [weak self] _ in
            guard let self else { return }
            self.queue.async {
                guard self.running, self.playbackGeneration == generation else { return }
                self.queuedPlaybackBufferCount = max(0, self.queuedPlaybackBufferCount - 1)
                if self.queuedPlaybackBufferCount == 0 {
                    // Re-prime after an actual underrun instead of immediately
                    // playing every late packet as a clicky, discontinuous clip.
                    self.playbackPrimed = false
                    self.playbackFadeSamplesRemaining = 40
                    self.player.pause()
                }
            }
        }
    }

    private func conditionMicrophoneFrame(_ frame: inout [Int16]) {
        guard !frame.isEmpty else { return }
        var filtered = [Float](repeating: 0, count: frame.count)
        var energy: Float = 0
        for index in frame.indices {
            let input = Float(frame[index]) / 32_768
            let output = 0.925 * (microphonePreviousOutput + input - microphonePreviousInput)
            microphonePreviousInput = input
            microphonePreviousOutput = output
            filtered[index] = output
            energy += output * output
        }
        let rms = sqrt(energy / Float(frame.count))
        if rms < max(0.02, microphoneNoiseFloor * 3) {
            microphoneNoiseFloor = microphoneNoiseFloor * 0.98 + rms * 0.02
        }
        let threshold = max(0.0035, microphoneNoiseFloor * 2.4)
        if rms >= threshold * 1.15 {
            microphoneGateOpen = true
            microphoneGateHoldFrames = 8
        } else if microphoneGateHoldFrames > 0 {
            microphoneGateHoldFrames -= 1
        } else if rms < threshold * 0.8 {
            microphoneGateOpen = false
        }
        // macOS voice processing already performs non-linear suppression.
        // Applying another gate causes metallic pumping, so the fallback
        // expander is active only when the system processing unit is absent.
        let targetGain: Float
        if systemVoiceProcessingActive || microphoneGateOpen || microphoneGateHoldFrames > 0 {
            targetGain = 1
        } else {
            targetGain = max(0.30, rms / threshold)
        }
        let smoothing: Float = targetGain > microphoneGateGain ? 0.45 : 0.06
        microphoneGateGain += (targetGain - microphoneGateGain) * smoothing
        let peak = filtered.reduce(Float.zero) { max($0, abs($1)) }
        let limiterGain: Float = peak > 0.90 ? 0.90 / peak : 1
        let gain = microphoneGateGain * limiterGain
        for index in frame.indices {
            let value = max(-0.95, min(0.95, filtered[index] * gain))
            frame[index] = Int16(value * 32_767)
        }
    }

    private func makeRingbackBuffer(format: AVAudioFormat) -> AVAudioPCMBuffer? {
        // UK ringing cadence: 0.4s tone, 0.2s silence, 0.4s tone, 2s silence.
        let duration = 3.0
        let frames = AVAudioFrameCount(format.sampleRate * duration)
        guard let buffer = AVAudioPCMBuffer(pcmFormat: format, frameCapacity: frames),
              let channels = buffer.floatChannelData else { return nil }
        buffer.frameLength = frames
        for frame in 0..<Int(frames) {
            let time = Double(frame) / format.sampleRate
            let toneOn = time < 0.4 || (time >= 0.6 && time < 1.0)
            let sample: Float = toneOn
                ? Float(0.10 * (sin(2 * .pi * 400 * time) + sin(2 * .pi * 450 * time)))
                : 0
            for channel in 0..<Int(format.channelCount) {
                channels[channel][frame] = sample
            }
        }
        return buffer
    }

    private func recoverFromAudioRouteChange() {
        guard running, !recoveringAudioRoute else { return }
        recoveringAudioRoute = true
        engine.stop()
        if tapInstalled {
            engine.inputNode.removeTap(onBus: 0)
            tapInstalled = false
        }
        microphoneTapGeneration &+= 1
        microphonePCM.removeAll(keepingCapacity: true)
        microphonePreviousInput = 0
        microphonePreviousOutput = 0
        microphoneGateGain = 1
        microphoneGateOpen = true
        microphoneGateHoldFrames = 0
        playbackFadeSamplesRemaining = 40
        let microphoneError = microphoneRequested ? installMicrophoneTapOnQueue() : nil
        do {
            try engine.start()
            resumePlayersAfterEngineStart()
            recoveringAudioRoute = false
            if microphoneError == nil {
                routeRecoveryAttempts = 0
                lastAudioRouteError = nil
            } else {
                lastAudioRouteError = microphoneError
                scheduleAudioRouteRecovery()
            }
        } catch {
            lastAudioRouteError = "音频设备切换后恢复失败：\(error.localizedDescription)"
            recoveringAudioRoute = false
            scheduleAudioRouteRecovery()
        }
    }

    private func resumePlayersAfterEngineStart() {
        if playbackPrimed, queuedPlaybackBufferCount > 0 {
            player.play()
        }
        if ringbackRunning {
            ringbackPlayer.play()
        }
    }

    private func scheduleAudioRouteRecovery() {
        routeRecoveryAttempts += 1
        guard running else { return }
        guard routeRecoveryAttempts <= 5 else {
            if lastAudioRouteError == nil {
                lastAudioRouteError = "无法恢复当前音频输入输出设备，请检查系统声音设置。"
            }
            return
        }
        queue.asyncAfter(deadline: .now() + 0.4) { [weak self] in
            self?.recoverFromAudioRouteChange()
        }
    }

    private func syncOnAudioQueue<T>(_ work: () -> T) -> T {
        if DispatchQueue.getSpecific(key: queueKey) != nil {
            return work()
        } else {
            return queue.sync(execute: work)
        }
    }

    private func defaultDeviceName(selector: AudioObjectPropertySelector) -> String {
        var address = AudioObjectPropertyAddress(
            mSelector: selector,
            mScope: kAudioObjectPropertyScopeGlobal,
            mElement: kAudioObjectPropertyElementMain)
        var deviceID: AudioDeviceID = 0
        var size = UInt32(MemoryLayout<AudioDeviceID>.size)
        guard AudioObjectGetPropertyData(
            AudioObjectID(kAudioObjectSystemObject), &address, 0, nil, &size, &deviceID) == noErr,
              deviceID != 0 else { return "系统默认设备" }
        var nameAddress = AudioObjectPropertyAddress(
            mSelector: kAudioDevicePropertyDeviceNameCFString,
            mScope: kAudioObjectPropertyScopeGlobal,
            mElement: kAudioObjectPropertyElementMain)
        var unmanagedName: Unmanaged<CFString>?
        size = UInt32(MemoryLayout<Unmanaged<CFString>?>.size)
        guard AudioObjectGetPropertyData(deviceID, &nameAddress, 0, nil, &size, &unmanagedName) == noErr,
              let unmanagedName else { return "系统默认设备" }
        return unmanagedName.takeRetainedValue() as String
    }
}
