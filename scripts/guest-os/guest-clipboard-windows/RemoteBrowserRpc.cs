using System.Text.Json.Nodes;

namespace Cage.GuestClipboard;

// Shared browser owner, independent of the interactive desktop service. Every
// child (Node, browser, renderers) remains inside the existing AppContainer job.
internal sealed class RemoteBrowserRpc(RemoteSandbox sandbox, Dictionary<string,string> environment) : IDisposable
{
    private readonly SemaphoreSlim gate = new(1, 1);
    private TerminalRpc.NativeTerminal? process;
    private ReadAheadStream? replies;
    private Task? errors;
    private string? inputOwner;
    private string Root
    {
        get
        {
            var installed = Path.Combine(Environment.GetFolderPath(Environment.SpecialFolder.ProgramFiles), "Browser Runtime", GuestContext.VM.ToString("D"));
            return sandbox.InstancePackage || Directory.Exists(installed) ? installed : Path.Combine(sandbox.ToolsDirectory, "chromium-headless-1254");
        }
    }
    internal bool Installed => new[] { "node.exe", "windows-remote-browser.mjs", "windows-browser.mjs", "playwright/index.mjs", "chrome-headless-shell-win64/chrome-headless-shell.exe" }.All(file => File.Exists(Path.Combine(Root, file)));

    internal async Task Run(Stream stream, CancellationToken token)
    {
        var lease = Guid.NewGuid().ToString("N");
        try
        {
            while (await Protocol.Read(stream, Protocol.WireLimit, token) is { } request)
            {
                if (request["operation"] != null || request["method"]?.GetValue<string>() is not ("GET" or "POST") ||
                    request["path"]?.GetValue<string>() is not { } route || !System.Text.RegularExpressions.Regex.IsMatch(route, "^/[a-z-]+$"))
                    throw new InvalidDataException("browser_request_invalid");
                JsonObject result;
                await gate.WaitAsync(token);
                try
                {
                    using var timeout = CancellationTokenSource.CreateLinkedTokenSource(token);
                    timeout.CancelAfter(TimeSpan.FromSeconds(30));
                    Start();
                    if (request["method"]!.GetValue<string>() == "POST")
                    {
                        if (inputOwner != null && inputOwner != lease)
                            await Control(new() { ["operation"] = "release-viewer" }, timeout.Token);
                        inputOwner = lease;
                    }
                    result = await Control(request, timeout.Token);
                }
                catch
                {
                    Close();
                    result = new() { ["error"] = "Windows 云端浏览器未能启动或响应", ["code"] = "browser_unavailable" };
                }
                finally { gate.Release(); }
                await Protocol.Write(stream, result, token);
            }
        }
        finally
        {
            await gate.WaitAsync(CancellationToken.None);
            try
            {
                if (inputOwner == lease && process != null)
                {
                    using var timeout = new CancellationTokenSource(TimeSpan.FromSeconds(3));
                    try { await Control(new() { ["operation"] = "release-viewer" }, timeout.Token); }
                    catch { Close(); }
                    inputOwner = null;
                }
            }
            finally { gate.Release(); }
        }
    }
    private void Start()
    {
        if (process != null) return;
        if (!Installed) throw new InvalidDataException("browser_components_missing");
        process = new TerminalRpc.NativeTerminal(80, 24, sandbox.Home,
            new(Path.Combine(Root, "node.exe"), [Path.Combine(Root, "windows-remote-browser.mjs")], sandbox.Home, environment, sandbox), pipes: true);
        replies = new ReadAheadStream(process.Output);
        var stderr = process.Error!;
        errors = Task.Run(async () =>
        {
            var buffer = new byte[4096];
            try { while (await stderr.ReadAsync(buffer) != 0) { } } catch (Exception) { }
        });
    }
    private async Task<JsonObject> Control(JsonObject request, CancellationToken token)
    {
        await Protocol.Write(process!.Input, request, token);
        return await Protocol.Read(replies!, Protocol.WireLimit, token) ?? throw new EndOfStreamException();
    }
    private void Close()
    {
        process?.Dispose(); process = null; replies?.Dispose(); replies = null; inputOwner = null;
        // stderr is drained independently, contains no public response, and is
        // closed with the owned job above. Observe its completion on shutdown.
        errors?.GetAwaiter().GetResult(); errors = null;
    }
    public void Dispose() { Close(); gate.Dispose(); }
}
