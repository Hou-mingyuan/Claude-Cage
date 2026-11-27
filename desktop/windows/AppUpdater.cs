using System.Diagnostics;
using System.Globalization;
using System.Reflection;
using System.Security.Cryptography;
using System.Text.Json;
using System.Threading.Channels;

namespace Cage.Windows;

internal sealed class AppUpdater : IDisposable
{
    public static string Version => typeof(AppUpdater).Assembly.GetCustomAttribute<AssemblyInformationalVersionAttribute>()?.InformationalVersion ?? "development";
    public static string Root => Path.Combine(Backend.Root, "updates");
    private readonly Action<Dictionary<string, object?>> changed;
    private readonly Action quit, reconnect;
    private readonly Func<string> readPublisher;
    private readonly Func<bool> canUpgradeUnsigned;
    private readonly Action<string>? report;
    private readonly Dictionary<string, object?> state = new() { ["phase"] = "idle", ["currentVersion"] = Version, ["message"] = "" };
    private CancellationTokenSource? cancellation;
    private InstallPlan? prepared;
    private bool commitRequested;
    private readonly bool standaloneCLI;
    private ChannelWriter<Dictionary<string, object?>>? cliProgress;

    public AppUpdater(Action<Dictionary<string, object?>> changed, Action quit, Action reconnect, Func<string>? readPublisher = null, Action<string>? report = null, Func<bool>? canUpgradeUnsigned = null, bool standaloneCLI = false)
    {
        this.changed = changed; this.quit = quit; this.reconnect = reconnect;
        this.readPublisher = readPublisher ?? Authenticode.OwnPublisher;
        this.canUpgradeUnsigned = canUpgradeUnsigned ?? DefaultCanUpgradeUnsigned;
        this.report = report;
        this.standaloneCLI = standaloneCLI;
        if (standaloneCLI) return;
        try
        {
            var result = Path.Combine(Root, "last-result.json");
            if (File.Exists(result) && new FileInfo(result).Length <= 16384)
            {
                var value = JsonDocument.Parse(File.ReadAllText(result)).RootElement;
                state["message"] = value.Text("message");
                if (value.TryGetProperty("success", out var success) && !success.GetBoolean()) { state["phase"] = "failed"; state["retryable"] = true; }
                try { File.Delete(result); } catch (IOException) { } catch (UnauthorizedAccessException) { }
            }
        }
        catch (Exception e) when (e is IOException or JsonException or UnauthorizedAccessException) { }
    }

    public Dictionary<string, object?> Handle(string operation)
    {
        if (cliProgress != null && operation != "status")
            return new(state) { ["message"] = "此次更新由终端发起，请在运行 cage update 的终端确认或取消" };
        if (Backend.ClientOnly)
        {
            if (state["phase"] as string != "manual" || operation != "status")
                Change("manual", "轻量客户端使用独立安装包更新；连接配置会保留，云端任务不受影响。");
            return new(state);
        }
        if (operation is "check" or "install" &&
            InCooldown())
            return new(state);
        switch (operation)
        {
            case "status": break;
            case "check": if (cancellation == null && prepared == null) _ = Run(true); break;
            case "install":
                if (cancellation == null)
                {
                    if (prepared != null) Ready(prepared);
                    else _ = Run(false);
                }
                break;
            case "complete": if (cancellation == null && prepared != null) _ = Install(); break;
            case "cancel": if (!commitRequested && state["phase"] as string != "installing") cancellation?.Cancel(); break;
            default: throw new InvalidDataException("更新操作无效");
        }
        return new(state);
    }

    private void Ready(InstallPlan plan)
    {
        state["latestVersion"] = plan.Version;
        state["officialUpgrade"] = plan.UnsignedSource;
        Change("ready", "更新已下载，请保存工作并手动停止环境后安装", true);
    }

    private bool InCooldown() => DateTimeOffset.TryParse(state.GetValueOrDefault("retryAt") as string,
        CultureInfo.InvariantCulture, DateTimeStyles.AssumeUniversal, out var retryAt) && retryAt > DateTimeOffset.UtcNow;

    private void Publish()
    {
        var snapshot = new Dictionary<string, object?>(state);
        changed(snapshot);
        cliProgress?.TryWrite(snapshot);
    }

    private void PublishDownloadState(JsonElement record, string? publisher, bool unsignedSource)
    {
        foreach (var property in record.EnumerateObject())
        {
            if (property.Name is not ("phase" or "message" or "currentVersion" or "latestVersion" or "receivedBytes" or "totalBytes" or "retryable" or "retryAt")) continue;
            state[property.Name] = property.Value.ValueKind switch {
                JsonValueKind.String => property.Value.GetString(),
                JsonValueKind.True => true, JsonValueKind.False => false,
                JsonValueKind.Number => property.Value.GetInt64(),
                _ => property.Value.Clone(),
            };
        }
        if (publisher == null && record.Text("phase") == "available")
            Change("manual", "当前 Windows 程序未签名或签名无法验证，请从官方发布页下载安装包，退出 Cage 后安装到原位置；配置、凭据、项目和虚拟机磁盘会保留。");
        else
        {
            state["officialUpgrade"] = record.Text("phase") != "current" && unsignedSource;
            Publish();
        }
    }

    // The desktop calls this on its UI thread, just like Handle. Only the
    // background writer touches the pipe while workbench events are published.
    public async Task<int> RunCLI(Stream input, Stream output, UpdateClients clients, CancellationToken lifetime = default)
    {
        if (cliProgress != null || cancellation != null || commitRequested)
            return await CliUpdate.Failure(output, "已有更新正在进行，请稍后重试");
        var states = Channel.CreateBounded<Dictionary<string, object?>>(new BoundedChannelOptions(16) {
            SingleReader = true, FullMode = BoundedChannelFullMode.DropOldest,
        });
        cliProgress = states.Writer;
        var writer = CliUpdate.WriteProgress(states.Reader, output, lifetime);
        try
        {
            if (Backend.ClientOnly) Change("manual", "轻量客户端请从官方发布页下载对应的更新安装包");
            else if (InCooldown()) Publish();
            else if (prepared != null) Ready(prepared);
            else
            {
                var download = Run(false);
                if (await Task.WhenAny(download, writer) == writer && !download.IsCompleted) cancellation?.Cancel();
                await download;
            }
            var phase = state.GetValueOrDefault("phase") as string;
            if (phase != "ready" || prepared == null)
            {
                if (phase is "current" or "manual") return 0;
                if (phase is not ("failed" or "waiting")) Change("failed", "未能取得经过验证的更新安装包", true);
                return 1;
            }
            var confirm = CliUpdate.ReadInstall(input, lifetime);
            if (await Task.WhenAny(confirm, writer) == writer || !await confirm)
            {
                states.Writer.TryWrite(CliUpdate.Status("canceled", "安装未确认，已保留原应用"));
                return 0;
            }
            await Install(clients, async () => {
                var handoff = new Dictionary<string, object?>(state) { ["phase"] = "handed_off", ["message"] = "安全安装程序已接手更新" };
                states.Writer.TryWrite(handoff); states.Writer.TryComplete();
                await writer.WaitAsync(TimeSpan.FromSeconds(5));
            });
            return commitRequested ? 0 : 1;
        }
        finally
        {
            states.Writer.TryComplete();
            try { await writer.WaitAsync(TimeSpan.FromSeconds(5)); }
            finally { cliProgress = null; }
        }
    }

    private static bool DefaultCanUpgradeUnsigned()
    {
        if (Environment.GetEnvironmentVariable("CAGE_DISABLE_OFFICIAL_UPGRADE") == "1") return false;
        try
        {
            var target = AppContext.BaseDirectory.TrimEnd(Path.DirectorySeparatorChar);
            if (string.IsNullOrEmpty(target)) return false;
            UpdatePolicy.PlainPath(target);
            var idPath = Path.Combine(target, "Cage.install-id");
            if (!File.Exists(idPath)) return false;
            UpdatePolicy.PlainPath(idPath);
            return File.ReadAllText(idPath).Trim() == "io.github.lee-xeo.claude-cage";
        }
        catch (Exception error) when (error is IOException or UnauthorizedAccessException or InvalidDataException)
        {
            return false;
        }
    }

    private void Change(string phase, string message, bool retryable = false, string retryAt = "")
    {
        if (phase is "current" or "manual") state["officialUpgrade"] = false;
        state["phase"] = phase; state["message"] = message; state["retryable"] = retryable; state["retryAt"] = retryAt; Publish();
    }

    public void MarkReady()
    {
        if (!Guid.TryParse(Environment.GetEnvironmentVariable("CAGE_UPDATE_CONFIRMATION"), out var token)) return;
        Directory.CreateDirectory(Root); UpdatePolicy.PlainPath(Root);
        File.WriteAllText(Path.Combine(Root, "launch-" + token + ".json"), JsonSerializer.Serialize(new { version = Version }));
        Change("current", "Cage 已更新至 " + Version);
        Environment.SetEnvironmentVariable("CAGE_UPDATE_CONFIRMATION", null);
    }

    private async Task Run(bool checkOnly)
    {
        // The backend reports progress at least once per retry. Bound its silence
        // rather than the transfer, so a slow connection can still finish.
        var silence = TimeSpan.FromMinutes(10);
        cancellation = new CancellationTokenSource(silence);
        var token = cancellation.Token;
        Change("checking", "正在检查更新，优先使用主机网络");
        string? archive = null;
        string? failure = null;
        string retryAt = "";
        using var process = new Process();
        var started = false;
        try
        {
            // Signed installs pin strictly to their own certificate. Eligible
            // unsigned source builds installed via the Cage installer may
            // upgrade in-place to the pinned official signed release; all other
            // unsigned builds fall back to manual download without auto-replacing.
            string? publisher = null;
            var unsignedSource = false;
            try { publisher = await Task.Run(readPublisher, token); }
            catch (Exception error) when (error is IOException or CryptographicException or UnauthorizedAccessException)
            {
                report?.Invoke("Windows publisher check failed: " + error.Message);
                if (await Task.Run(canUpgradeUnsigned, token))
                {
                    publisher = Authenticode.OfficialPublisherKey;
                    unsignedSource = true;
                }
            }
            token.ThrowIfCancellationRequested();
            process.StartInfo = Backend.StartInfo(Backend.Executable, "app-update", checkOnly || publisher == null ? "check" : "download");
            if (!process.Start()) throw new IOException("无法启动更新检查");
            started = true;
            using var abort = token.Register(() => { try { if (!process.HasExited) process.Kill(); } catch (InvalidOperationException) { } });
            var errors = Backend.Drain(process.StandardError, token);
            JsonElement last = default;
            while (await process.StandardOutput.ReadLineAsync(token) is { } line)
            {
                if (line.Length > 65536) throw new IOException("更新响应超过允许大小");
                cancellation.CancelAfter(silence);
                using var document = JsonDocument.Parse(line);
                last = document.RootElement.Clone();
                retryAt = last.Text("retryAt");
                // Only the native installer can authorize installation or acknowledge
                // handoff. A terminal result from the downloader also needs a clean exit.
                var phase = last.Text("phase");
                if (phase is "checking" or "downloading" or "downloaded") PublishDownloadState(last, publisher, unsignedSource);
                else if (phase is not ("current" or "available" or "waiting" or "failed"))
                    throw new IOException("下载程序返回了无法识别的更新状态");
            }
            await process.WaitForExitAsync(token); await errors;
            if (process.ExitCode != 0) throw new IOException(last.ValueKind == JsonValueKind.Object && last.Text("message") is { Length: > 0 } error ? error : "更新请求未完成");
            if (last.ValueKind != JsonValueKind.Object) throw new IOException("更新检查未返回完整结果，请重试");
            var result = last.Text("phase");
            if (result is "current" or "waiting" or "failed" || (result == "available" && (checkOnly || publisher == null)))
            {
                PublishDownloadState(last, publisher, unsignedSource);
                return;
            }
            if (result != "downloaded" || checkOnly || publisher == null) throw new IOException("未能取得完整的更新安装包");
            archive = UpdatePolicy.DownloadPath(last.Text("archivePath"), Root);
            var version = last.Text("latestVersion"); var digest = last.Text("sha256");
            if (!UpdatePolicy.IsNewer(version, Version)) throw new IOException("安装包版本无效或不高于当前版本");
            cancellation.CancelAfter(TimeSpan.FromMinutes(25));
            Change("verifying", "正在验证 Windows 签名和安装包内容");
            await Task.Run(() => UpdateInstaller.VerifyArchive(archive, digest, version, publisher), token);
            token.ThrowIfCancellationRequested();
            prepared = new InstallPlan(archive, digest, version, publisher,
                AppContext.BaseDirectory.TrimEnd(Path.DirectorySeparatorChar), Environment.ProcessId,
                Process.GetCurrentProcess().StartTime.ToUniversalTime().Ticks, Guid.NewGuid().ToString(), unsignedSource);
            archive = null;
        }
        catch (OperationCanceledException) { failure = "更新已暂停，可稍后重试"; retryAt = ""; }
        catch (Exception error) { failure = error.Message; }
        finally
        {
            if (started)
            {
                if (!process.HasExited) process.Kill();
                // Windows can expose an exit code before the process handle signals and releases its files.
                if (!process.WaitForExit(0)) await Task.Run(process.WaitForExit);
            }
            if (archive != null) { try { Directory.Delete(Path.GetDirectoryName(archive)!, true); } catch (IOException) { } }
            if (failure != null) Change("failed", failure, true, retryAt);
            cancellation.Dispose(); cancellation = null;
        }
        if (prepared != null) Ready(prepared);
    }

    private async Task Install(UpdateClients? clients = null, Func<Task>? handedOff = null)
    {
        if (prepared == null) return;
        cancellation = new CancellationTokenSource(TimeSpan.FromMinutes(5));
        var token = cancellation.Token;
        Process? helper = null;
        var helperPath = Path.Combine(Root, "helper-" + Guid.NewGuid() + ".exe");
        state["officialUpgrade"] = prepared.UnsignedSource;
        Change("installing", "正在准备安装更新");
        try
        {
            // Validate the destination before stopping a running environment.
            UpdatePolicy.PlainPath(prepared.Target);
            UpdateRecovery.EnsureNoPendingTransaction(prepared.Target);
            if (prepared.UnsignedSource && !string.Equals(prepared.Publisher, Authenticode.OfficialPublisherKey, StringComparison.Ordinal))
                throw new IOException("安装计划发布者不匹配");
            Directory.CreateDirectory(Root); UpdatePolicy.PlainPath(Root);
            File.Copy(Environment.ProcessPath!, helperPath);
            if (!prepared.UnsignedSource)
                await Task.Run(() => Authenticode.SamePublisher(helperPath, prepared.Publisher), token);
            var start = Backend.StartInfo(helperPath, "--install-update");
            // A live process's working directory cannot be renamed on Windows.
            // The helper outlives this App and must not retain its install path.
            start.WorkingDirectory = Root;
            start.RedirectStandardInput = true;
            helper = Process.Start(start) ?? throw new IOException("无法启动安全安装程序");
            await helper.StandardInput.WriteLineAsync(JsonSerializer.Serialize(prepared with { Clients = clients }));
            await helper.StandardInput.FlushAsync(token);
            var ready = await helper.StandardOutput.ReadLineAsync(token);
            if (ready == "BUSY") throw new IOException("另一个配置正在更新此安装，请稍后重试");
            if (ready != "READY") throw new IOException("安装程序准备失败，原应用已保留");
            var (exit, data) = await Backend.Json(["prepare-update", "--json"], token);
            if (data.Text("status") == "stop_required")
            {
                throw new IOException(clients == null ? "环境仍在运行，请在工作台停止环境后重试安装" : "环境仍在运行，请先执行 cage stop，再运行 cage update");
            }
            if (exit != 0 || data.Text("status") != "prepared") throw new IOException(data.Text("message") is { Length: > 0 } detail ? detail : "环境未能安全停止");
            if (helper.HasExited) throw new IOException("安装程序已退出，原应用已保留");
            // A broken pipe or deadline after sending may already have delivered
            // COMMIT. Once the write begins, the helper alone owns the outcome.
            commitRequested = true;
            try
            {
                await helper.StandardInput.WriteLineAsync("COMMIT"); await helper.StandardInput.FlushAsync(token);
                helper.StandardInput.Close();
                Change("installing", "正在安装并重启 Cage");
                if (handedOff != null)
                {
                    if (await helper.StandardOutput.ReadLineAsync(token) != "COMMITTED") throw new IOException("未收到安全安装程序的接手确认，请检查更新结果");
                    await handedOff();
                }
            }
            catch (Exception error)
            {
                report?.Invoke("Windows update handoff could not be confirmed: " + error.Message);
                if (handedOff != null) throw new IOException("未收到安全安装程序的接手确认，请查看更新结果", error);
            }
            finally { quit(); }
        }
        catch (Exception error) when (!commitRequested) { Change("failed", error is OperationCanceledException ? "安装准备超时，原应用已保留" : error.Message, true); reconnect(); }
        finally
        {
            if (!commitRequested && helper != null)
            {
                helper.StandardInput.Close();
                try { await helper.WaitForExitAsync().WaitAsync(TimeSpan.FromSeconds(5)); }
                catch (TimeoutException) { if (!helper.HasExited) helper.Kill(); }
            }
            helper?.Dispose();
            if (!commitRequested) { try { File.Delete(helperPath); } catch (IOException) { } }
            cancellation?.Dispose(); cancellation = null;
        }
    }

    public void Dispose()
    {
        if (commitRequested) return;
        cancellation?.Cancel();
        if (!standaloneCLI || prepared == null) return;
        // A cold CLI has no window retaining this verified package for a later confirmation.
        try
        {
            var path = UpdatePolicy.DownloadPath(prepared.Archive, Root);
            UpdatePolicy.PlainPath(path);
            File.Delete(path);
            Directory.Delete(Path.GetDirectoryName(path)!);
        }
        catch (Exception error) when (error is IOException or UnauthorizedAccessException or InvalidDataException) { }
    }
}

internal sealed record InstallPlan(string Archive, string Digest, string Version, string Publisher,
    string Target, int ParentPid, long ParentStartTicks, string Confirmation, bool UnsignedSource = false, UpdateClients? Clients = null);
