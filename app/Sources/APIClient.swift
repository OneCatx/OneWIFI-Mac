import Foundation

/// 基于 URLSession + UnixSocketURLProtocol 的 API 客户端。
/// 请求走 http+unix:// 协议，socket 路径由 BackendProcess 统一提供。
struct APIClient {
    private static let base = URL(string: "http+unix://proletarius")!

    private let session: URLSession
    private let timeoutInterval: TimeInterval
    private let encoder = JSONEncoder()

    init(timeoutInterval: TimeInterval = 30) {
        self.timeoutInterval = timeoutInterval
        let config = URLSessionConfiguration.ephemeral
        config.protocolClasses = [UnixSocketURLProtocol.self]
        config.timeoutIntervalForRequest = timeoutInterval
        config.timeoutIntervalForResource = max(60, timeoutInterval)
        session = URLSession(configuration: config)

        encoder.dateEncodingStrategy = .iso8601
    }

    /// 每个响应独立解码，formatter 只在此次同步 decode 内复用，不跨并发请求共享。
    static func decodeResponse<T: Decodable>(_ type: T.Type, from data: Data) throws -> T {
        let decoder = JSONDecoder()
        let withFraction = ISO8601DateFormatter()
        withFraction.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        let plain = ISO8601DateFormatter()
        plain.formatOptions = [.withInternetDateTime]

        // Go 端 time.Time 输出 RFC3339Nano，小数秒位数不固定（1-9 位）
        // 优先解析原始值；格式化器不接受小数秒精度时，保留截断到 3 位的回退。
        decoder.dateDecodingStrategy = .custom { decoder in
            let container = try decoder.singleValueContainer()
            let str = try container.decode(String.self)
            if let d = withFraction.date(from: str) { return d }
            if let d = plain.date(from: str) { return d }
            // Go 的 RFC3339Nano 可能输出 6-9 位小数秒，截断为 3 位再解析
            if let dotRange = str.range(of: ".") {
                let beforeFraction = String(str[..<dotRange.lowerBound])
                let afterDot = String(str[dotRange.upperBound...])
                // 提取时区部分（Z 或 +xx:xx / -xx:xx）
                let tzStart: String.Index
                if let zIdx = afterDot.firstIndex(of: "Z") {
                    tzStart = zIdx
                } else if let plusIdx = afterDot.range(of: #"[\+\-]\d{2}:\d{2}$"#, options: .regularExpression)?.lowerBound {
                    tzStart = plusIdx
                } else {
                    throw DecodingError.dataCorruptedError(
                        in: container, debugDescription: "无法解析时间：\(str)")
                }
                let fractionDigits = String(afterDot[..<tzStart])
                let tzPart = String(afterDot[tzStart...])
                let truncatedFraction = String(fractionDigits.prefix(3))
                let rebuilt = beforeFraction + "." + truncatedFraction + tzPart
                if let d = withFraction.date(from: rebuilt) { return d }
            }
            throw DecodingError.dataCorruptedError(
                in: container, debugDescription: "无法解析时间：\(str)")
        }

        return try decoder.decode(type, from: data)
    }

    func get<T: Decodable>(_ path: String, as type: T.Type = T.self) async throws -> T {
        let url = Self.makeURL(path)
        var request = URLRequest(url: url)
        request.httpMethod = "GET"
        Self.authorize(&request)
        let (data, response) = try await session.data(for: request)
        try Self.validate(response, data: data)
        return try Self.decodeResponse(T.self, from: data)
    }

    func getData(_ path: String) async throws -> Data {
        let url = Self.makeURL(path)
        var request = URLRequest(url: url)
        request.httpMethod = "GET"
        Self.authorize(&request)
        let (data, response) = try await session.data(for: request)
        try Self.validate(response, data: data)
        return data
    }

    func upload<T: Decodable>(
        _ path: String,
        data: Data,
        contentType: String = "application/json",
        as type: T.Type = T.self
    ) async throws -> T {
        let url = Self.makeURL(path)
        var request = URLRequest(url: url)
        request.httpMethod = "POST"
        Self.authorize(&request)
        request.httpBody = data
        request.setValue(contentType, forHTTPHeaderField: "Content-Type")
        let (responseData, response) = try await session.data(for: request)
        try Self.validate(response, data: responseData)
        return try Self.decodeResponse(T.self, from: responseData)
    }

    func send<T: Decodable>(
        _ path: String,
        method: String = "POST",
        body: (any Encodable)? = nil,
        as type: T.Type = T.self
    ) async throws -> T {
        let url = Self.makeURL(path)
        var request = URLRequest(url: url)
        request.httpMethod = method
        Self.authorize(&request)
        if let body {
            request.httpBody = try encoder.encode(body)
            request.setValue("application/json", forHTTPHeaderField: "Content-Type")
        }
        let (data, response) = try await performMutation(request)
        try Self.validate(response, data: data)
        do {
            return try Self.decodeResponse(T.self, from: data)
        } catch {
            // A 2xx write response may arrive after the operation committed.
            // A malformed payload must not encourage sending the write again.
            guard Self.mutationTimeout(path: path, method: method, defaultTimeout: 0) > 0 else { throw error }
            throw APIOperationOutcomeUnknownError(reason: "本机服务已响应，但无法解析操作结果")
        }
    }

    func send(_ path: String, method: String = "POST", body: (any Encodable)? = nil) async throws {
        let url = Self.makeURL(path)
        var request = URLRequest(url: url)
        request.httpMethod = method
        Self.authorize(&request)
        if let body {
            request.httpBody = try encoder.encode(body)
            request.setValue("application/json", forHTTPHeaderField: "Content-Type")
        }
        let (data, response) = try await performMutation(request)
        try Self.validate(response, data: data)
    }

    /// Only known long-running card/SMS writes receive a larger budget. GETs,
    /// Ordinary control requests keep their existing limits.
    static func mutationTimeout(path: String, method: String, defaultTimeout: TimeInterval) -> TimeInterval {
        let path = "/" + (path.split(separator: "?", maxSplits: 1, omittingEmptySubsequences: false).first ?? "")
            .trimmingCharacters(in: CharacterSet(charactersIn: "/"))
        let longWrite: Bool
        switch method.uppercased() {
        case "POST":
            longWrite = ["/api/sms/send", "/api/esim/switch", "/api/esim/download",
                         "/api/reader/esim/switch", "/api/reader/esim/download",
                         "/api/reader/vowifi", "/api/vowifi"].contains(path)
        case "PATCH", "DELETE":
            longWrite = ["/api/esim/profile", "/api/reader/esim/profile"].contains(path)
        default:
            longWrite = false
        }
        return longWrite ? max(190, defaultTimeout) : defaultTimeout
    }

    private func performMutation(_ original: URLRequest) async throws -> (Data, URLResponse) {
        var request = original
        let timeout = Self.mutationTimeout(
            path: request.url?.path ?? "", method: request.httpMethod ?? "POST", defaultTimeout: timeoutInterval)
        request.timeoutInterval = timeout
        let isLongWrite = Self.mutationTimeout(
            path: request.url?.path ?? "", method: request.httpMethod ?? "POST", defaultTimeout: 0) > 0
        let requestSession: URLSession
        if timeout > session.configuration.timeoutIntervalForResource {
            let configuration = session.configuration
            configuration.timeoutIntervalForRequest = timeout
            configuration.timeoutIntervalForResource = timeout
            requestSession = URLSession(configuration: configuration)
        } else {
            requestSession = session
        }
        defer { if requestSession !== session { requestSession.finishTasksAndInvalidate() } }
        do {
            return try await requestSession.data(for: request)
        } catch let error as URLError where isLongWrite
            && [.timedOut, .networkConnectionLost, .badServerResponse].contains(error.code) {
            throw APIOperationOutcomeUnknownError()
        }
    }

    private static func validate(_ response: URLResponse, data: Data) throws {
        guard let http = response as? HTTPURLResponse else { return }
        guard (200..<300).contains(http.statusCode) else {
            if let payload = try? JSONDecoder().decode(APIErrorPayload.self, from: data),
               !payload.error.isEmpty {
                throw APIError.server(
                    http.statusCode,
                    code: payload.code,
                    message: payload.error,
                    recoverable: payload.recoverable ?? false)
            }
            throw APIError.httpStatus(http.statusCode)
        }
    }

    private static func authorize(_ request: inout URLRequest) {
        request.setValue(
            LocalBackendAuthentication.appToken,
            forHTTPHeaderField: LocalBackendAuthentication.headerName)
    }

    private static func makeURL(_ path: String) -> URL {
        let components = path.split(separator: "?", maxSplits: 1, omittingEmptySubsequences: false)
        let cleanPath = String(components[0])
        let url = base.appendingPathComponent(cleanPath)
        guard components.count == 2,
              var urlComponents = URLComponents(url: url, resolvingAgainstBaseURL: false) else {
            return url
        }
        urlComponents.percentEncodedQuery = String(components[1])
        return urlComponents.url ?? url
    }
}

struct APIOperationOutcomeUnknownError: LocalizedError {
    var reason = "等待本机服务响应时超时或连接中断"
    var errorDescription: String? {
        "操作结果待确认：\(reason)。请刷新操作状态或发送记录，确认前不要重复提交。"
    }
}

func operationOutcomeIsUnknown(_ error: any Error) -> Bool {
    if error is APIOperationOutcomeUnknownError { return true }
    if let apiError = error as? APIError, apiError.code?.hasSuffix("outcome_unknown") == true { return true }
    return error.localizedDescription.contains("结果待确认")
}

func smsSendErrorMessage(_ error: any Error) -> String {
    operationOutcomeIsUnknown(error) ? userFacingErrorMessage(error) : "发送失败：\(userFacingErrorMessage(error))"
}

private struct APIErrorPayload: Decodable {
    let error: String
    let code: String?
    let recoverable: Bool?
}

enum APIError: LocalizedError {
    case httpStatus(Int)
    case server(Int, code: String?, message: String, recoverable: Bool)

    var statusCode: Int {
        switch self {
        case .httpStatus(let code), .server(let code, _, _, _): return code
        }
    }

    var code: String? {
        switch self {
        case .httpStatus: return nil
        case .server(_, let code, _, _): return code
        }
    }

    var isRecoverable: Bool {
        switch self {
        case .httpStatus: return true
        case .server(_, _, _, let recoverable): return recoverable
        }
    }

    var errorDescription: String? {
        switch self {
        case .httpStatus(let code): return "服务返回错误（HTTP \(code)）"
        case .server(_, _, let message, _): return localizedBackendMessage(message)
        }
    }
}

/// 将后端或系统返回的常见技术错误转换为可直接操作的中文提示。
func userFacingErrorMessage(_ error: any Error) -> String {
    if let apiError = error as? APIError {
        return apiError.errorDescription ?? "请求失败，请稍后重试"
    }
    if let urlError = error as? URLError {
        switch urlError.code {
        case .timedOut:
            return "请求超时，请检查设备连接后重试"
        case .cannotConnectToHost, .networkConnectionLost, .notConnectedToInternet:
            return "无法连接本机服务，请确认 OneWIFI 后端正在运行"
        case .cancelled:
            return "操作已取消"
        default:
            break
        }
    }
    return localizedBackendMessage(error.localizedDescription)
}

private func localizedBackendMessage(_ message: String) -> String {
    let normalized = message.lowercased()
    if normalized.contains("dji usb device is not connected")
        || normalized.contains("no usb at device found") {
        return "未检测到 DJI 模块，请检查 USB 连接"
    }
    if normalized.contains("at serial port is unavailable")
        || normalized.contains("usb at device is not open") {
        return "DJI 模块短信通道尚未就绪，请稍后重试"
    }
    if normalized.contains("usb at is cooling down") {
        return "DJI 模块正在重新连接，请稍后重试"
    }
    if normalized.contains("phone and message are required") {
        return "请填写收件人号码和短信内容"
    }
    if normalized.contains("sender is required") {
        return "缺少短信联系人号码"
    }
    if normalized.contains("module sms deletion is only available through usb at") {
        return "DJI 模块短信通道未就绪，暂时无法删除模块内短信"
    }
    return message
}

// MARK: - 模块连接状态检测工具

/// 判断错误消息是否表示 DJI 模块未连接
func isModuleDisconnectedError(_ message: String) -> Bool {
    message.localizedCaseInsensitiveContains("not connected")
        || message.localizedCaseInsensitiveContains("USB device")
        || message.localizedCaseInsensitiveContains("未检测到 DJI 模块")
        || message.localizedCaseInsensitiveContains("未连接")
}

/// 模块未连接时的友好提示文案
let moduleDisconnectedFriendlyMessage = "模块未连接，请确认 DJI 模块已通过 USB 连接"
