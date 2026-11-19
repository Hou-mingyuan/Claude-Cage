using System.Security.Cryptography;
using System.Text.Json.Nodes;

namespace Cage.GuestClipboard;

// Separate authenticated background endpoint. Never advertises a desktop,
// weakens SessionGuard, or routes commands through an unrestricted user shell.
internal static class RemoteHeadless
{
    internal static async Task Run(Guid instance, string sid, string key, Guid machine, bool instancePackage, CancellationToken token)
    {
        using var lifetime = CancellationTokenSource.CreateLinkedTokenSource(token);
        token = lifetime.Token;
        LocalGuestTransport.VerifyMachine(machine);
        using var sandbox = new RemoteSandbox(instance, sid, instancePackage);
        sandbox.AllowReadTools();
        var environment = sandbox.WorkloadEnvironment();
        using var browser = new RemoteBrowserRpc(sandbox, environment);
        var secret = Pairing.Load(key, instance, sid, false);
        var session = "headless-" + Guid.NewGuid().ToString("N");
        var active = new HashSet<Task>();
        using var slots = new SemaphoreSlim(12);
        try
        {
            while (!token.IsCancellationRequested)
            {
                await slots.WaitAsync(token);
                var pipe = LocalGuestTransport.Listen(instance, sid, true);
                try { await pipe.WaitForConnectionAsync(token); }
                catch { pipe.Dispose(); slots.Release(); throw; }
                var task = Task.Run(async () =>
                {
                    try { await Connection(pipe, sandbox, environment, browser, instance, sid, secret, session, token); }
                    finally { pipe.Dispose(); slots.Release(); }
                }, CancellationToken.None);
                active.RemoveWhere(t => t.IsCompleted);
                active.Add(task);
            }
        }
        catch (OperationCanceledException) when (token.IsCancellationRequested) { }
        finally
        {
            lifetime.Cancel();
            await Task.WhenAll(active);
            CryptographicOperations.ZeroMemory(secret);
        }
    }

    private static async Task Connection(Stream network, RemoteSandbox sandbox, Dictionary<string,string> environment, RemoteBrowserRpc browser,
        Guid instance, string sid, byte[] secret, string session, CancellationToken token)
    {
        using var stream = new ReadAheadStream(network);
        using var handshake = CancellationTokenSource.CreateLinkedTokenSource(token);
        handshake.CancelAfter(TimeSpan.FromSeconds(10));
        var authenticated = false;
        try
        {
            var nonce = Convert.ToBase64String(RandomNumberGenerator.GetBytes(32));
            await Protocol.Write(stream, new() { ["nonce"] = nonce, ["vm_id"] = instance.ToString("D"), ["sid"] = sid,
                ["session"] = session, ["proof"] = Protocol.Proof(secret, "server", nonce, instance, sid, session, "", null) }, handshake.Token);
            var request = await Protocol.Read(stream, 8192, handshake.Token) ?? throw new EndOfStreamException();
            var operation = request["operation"]?.GetValue<string>() ?? "";
            if (operation is not ("headless-status" or "headless-terminal" or "headless-browser" or "headless-safety" or "headless-cli") || request["paths"] != null ||
                request["vm_id"]?.GetValue<string>() != instance.ToString("D") || request["sid"]?.GetValue<string>() != sid ||
                !Protocol.EqualProof(Protocol.Proof(secret, "client", nonce, instance, sid, session, operation, null), request["proof"]?.GetValue<string>()))
                throw new InvalidDataException("headless_pairing_invalid");
            authenticated = true;
            await Protocol.Write(stream, new() { ["version"] = 1, ["os"] = "windows", ["vm_id"] = instance.ToString("D"),
                ["gui_user_sid"] = sid, ["session"] = session, ["available"] = true, ["execution"] = "appcontainer-headless",
                ["process_session"] = 0, ["sandbox_sid"] = sandbox.SID, ["capabilities"] = new JsonArray("headless-terminal", "headless-browser", "headless-cli"),
                ["components"] = new JsonObject { ["cli"] = WindowsCliRpc.ComponentStatus() },
                ["browser"] = new JsonObject { ["installed"] = browser.Installed, ["network_configured"] = false },
                ["workload_ready"] = false, ["desktop_available"] = false, ["home"] = sandbox.Home }, token);
            if (operation == "headless-status") return;
            if (operation == "headless-safety") { await RemoteSafetyRpc.Run(stream, sandbox, environment, session, token); return; }
            if (operation == "headless-browser") { await browser.Run(stream, token); return; }
            if (operation == "headless-cli") { await WindowsCliRpc.RunHeadless(stream, token, sandbox); return; }
            await TerminalRpc.RunHeadless(stream, token, new(RemoteSandbox.ShellPath(),
                ["-NoLogo", "-NoProfile", "-WorkingDirectory", sandbox.Home, "-NoExit", "-File", Path.Combine(AppContext.BaseDirectory, "remote-shell.ps1")],
                sandbox.Home, environment, sandbox));
        }
        catch (Exception)
        {
            if (authenticated)
                try
                {
                    using var timeout = new CancellationTokenSource(TimeSpan.FromSeconds(1));
                    await Protocol.Write(stream, new() { ["error"] = "headless_operation_failed" }, timeout.Token);
                }
                catch { }
        }
    }
}
