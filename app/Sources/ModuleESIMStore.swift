import Combine
import Foundation

@MainActor
final class ModuleESIMStore: ObservableObject {
    @Published private(set) var overview: ESIMOverview?
    @Published private(set) var profileNotes: [String: ProfileNote] = [:]
    @Published private(set) var operation: ESIMOperationSnapshot?
    @Published private(set) var busy = false
    @Published private(set) var error: String?
    @Published private(set) var message: String?

    private var generation: UInt64 = 0
    private var task: Task<Void, Never>?
    private var pollTask: Task<Void, Never>?
    private var notesTask: Task<Void, Never>?

    deinit {
        task?.cancel()
        pollTask?.cancel()
        notesTask?.cancel()
    }

    var profiles: [ESIMProfileContext] {
        (overview?.profiles ?? []).flatMap { group in
            (group.profiles ?? []).map {
                ESIMProfileContext(eid: group.eid ?? "", aidHex: group.aidHex ?? "", profile: $0)
            }
        }
    }

    func savedPhoneNumber(for iccid: String) -> String? {
        let target = normalizedICCID(iccid)
        guard !target.isEmpty else { return nil }
        return usableSIMPhoneNumber(profileNotes.first(where: { normalizedICCID($0.key) == target })?.value.phone)
    }

    private func normalizedICCID(_ value: String) -> String {
        value.filter(\.isNumber)
    }

    func clear() {
        generation &+= 1
        task?.cancel()
        pollTask?.cancel()
        task = nil
        pollTask = nil
        notesTask?.cancel()
        notesTask = nil
        overview = nil
        profileNotes = [:]
        operation = nil
        busy = false
        error = nil
        message = nil
    }

    func load(refresh: Bool = false) {
        guard !busy else { return }
        generation &+= 1
        let current = generation
        task?.cancel()
        busy = true
        error = nil
        task = Task { [weak self] in
            guard let self else { return }
            defer {
                if self.generation == current {
                    self.busy = false
                    self.task = nil
                }
            }
            do {
                let path = refresh ? "api/esim?refresh=true" : "api/esim"
                let result: ESIMOverview = try await APIClient(timeoutInterval: 45).get(path)
                guard !Task.isCancelled, self.generation == current else { return }
                self.overview = result
                self.operation = result.operation
                if result.operation?.isActive == true { self.pollOperation(generation: current) }
                self.loadProfileNotes(generation: current)
            } catch {
                guard !Task.isCancelled, self.generation == current else { return }
                self.error = error.localizedDescription
            }
        }
    }

    private func loadProfileNotes(generation current: UInt64) {
        notesTask?.cancel()
        notesTask = Task { [weak self] in
            guard let self else { return }
            let result: NotesResponse? = try? await APIClient(timeoutInterval: 8).get("api/esim/notes")
            guard !Task.isCancelled, self.generation == current else { return }
            self.profileNotes = result?.notes ?? [:]
            self.notesTask = nil
        }
    }

    func switchProfile(_ item: ESIMProfileContext) {
        guard !busy, operation?.isActive != true,
              overview?.capabilities?.canSwitch == true, !item.profile.isEnabled else { return }
        perform("切换 Profile", expectedSwitchICCID: item.profile.iccid) {
            let result: ESIMSwitchResult = try await APIClient(timeoutInterval: 130).send(
                "api/esim/switch", body: ESIMSwitchRequest(iccid: item.profile.iccid, aid: item.aidHex))
            return result.operation
        }
    }

    func deleteProfile(_ item: ESIMProfileContext) {
        guard !busy, operation?.isActive != true,
              overview?.capabilities?.canDelete == true, !item.profile.isEnabled else { return }
        perform("删除 Profile") {
            let _: ESIMProfileResult = try await APIClient(timeoutInterval: 90).send(
                "api/esim/profile", method: "DELETE",
                body: ESIMDeleteRequest(iccid: item.profile.iccid, aid: item.aidHex))
            return nil
        }
    }

    func download(_ request: ESIMDownloadRequest) {
        guard !busy, operation?.isActive != true,
              overview?.capabilities?.canDownload == true else { return }
        perform("下载 Profile") {
            let started: ESIMOperationSnapshot = try await APIClient(timeoutInterval: 45)
                .send("api/esim/download", body: request)
            return started
        }
    }

    func confirmDownload(operationID: String, confirm: Bool) {
        guard operation?.id == operationID, operation?.awaitsDownloadConfirmation == true else { return }
        let current = generation
        Task { [weak self] in
            guard let self else { return }
            do {
                let updated: ESIMOperationSnapshot = try await APIClient(timeoutInterval: 10).send(
                    "api/esim/download/confirm",
                    body: ESIMDownloadConfirmationRequest(operationID: operationID, confirm: confirm))
                guard generation == current, updated.id == operationID else { return }
                operation = updated
            } catch {
                guard generation == current else { return }
                self.error = "无法提交下载确认：\(error.localizedDescription)"
            }
        }
    }

    private func perform(
        _ title: String,
        expectedSwitchICCID: String? = nil,
        action: @escaping () async throws -> ESIMOperationSnapshot?
    ) {
        generation &+= 1
        let current = generation
        task?.cancel()
        pollTask?.cancel()
        busy = true
        error = nil
        message = nil
        task = Task { [weak self] in
            guard let self else { return }
            do {
                let result = try await action()
                guard !Task.isCancelled, self.generation == current else { return }
                self.busy = false
                self.task = nil
                self.operation = result
                if result?.isActive == true {
                    self.pollOperation(generation: current)
                } else {
                    self.message = "\(title)已提交"
                    self.load(refresh: true)
                }
            } catch {
                guard !Task.isCancelled, self.generation == current else { return }
                // A successful switch restarts the modem. The socket may drop
                // before the synchronous response reaches us, so query the
                // operation before suggesting another destructive write.
                if let expectedSwitchICCID,
                   let recovered: ESIMOperationResponse = try? await APIClient(timeoutInterval: 10)
                       .get("api/esim/operation"),
                   let operation = recovered.operation,
                   operation.kind == "switch_profile",
                   operation.targetICCID == expectedSwitchICCID,
                   self.generation == current {
                    self.busy = false
                    self.task = nil
                    self.operation = operation
                    if operation.isActive {
                        self.pollOperation(generation: current)
                    } else if operation.isSuccessful || operation.hasWarning {
                        self.message = operation.message ?? "Profile 已切换"
                        self.load(refresh: true)
                    } else {
                        self.error = operation.error ?? "切换 Profile 失败"
                    }
                    return
                }
                self.busy = false
                self.task = nil
                self.error = operationOutcomeIsUnknown(error)
                    ? "\(title)结果待确认：\(error.localizedDescription)"
                    : "\(title)失败：\(error.localizedDescription)"
            }
        }
    }

    private func pollOperation(generation current: UInt64) {
        pollTask?.cancel()
        pollTask = Task { [weak self] in
            guard let self else { return }
            // A module download can run for the backend's full five-minute
            // transfer window. Keep polling long enough to receive its real
            // terminal error instead of showing a false timeout at 3 minutes.
            for _ in 0..<180 {
                try? await Task.sleep(for: .seconds(2))
                guard !Task.isCancelled, self.generation == current else { return }
                do {
                    let result: ESIMOperationResponse = try await APIClient(timeoutInterval: 15)
                        .get("api/esim/operation")
                    guard !Task.isCancelled, self.generation == current else { return }
                    self.operation = result.operation
                    if let result = result.operation, !result.isActive {
                        self.pollTask = nil
                        if result.isFailed {
                            self.error = result.error ?? "eSIM 操作失败"
                        } else {
                            self.message = result.message ?? "eSIM 操作已完成"
                            self.load(refresh: true)
                        }
                        return
                    }
                } catch {
                    // Module restart can briefly interrupt the service socket.
                }
            }
            guard !Task.isCancelled, self.generation == current else { return }
            self.pollTask = nil
            self.error = "eSIM 操作状态读取超时，请刷新确认，不要立即重复写卡"
        }
    }
}
