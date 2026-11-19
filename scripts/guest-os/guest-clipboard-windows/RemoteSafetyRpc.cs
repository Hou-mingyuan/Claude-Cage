using System.Text.Json.Nodes;
using System.Security.AccessControl;
using System.Security.Principal;

namespace Cage.GuestClipboard;

// The paired bridge only launches the fixed probe. All observations execute in
// the same verified AppContainer token, desktop and job as the real terminal.
internal static class RemoteSafetyRpc
{
    internal static async Task Run(Stream stream, RemoteSandbox sandbox, Dictionary<string,string> environment,
        string session, CancellationToken token)
    {
        var input = await Protocol.Read(stream, 16384, token) ?? throw new EndOfStreamException();
        if (input["stage"]?.GetValue<string>() is not ("environment" or "boundary" or "network"))
            throw new InvalidDataException("remote_safety_stage_invalid");
        var script = Path.Combine(AppContext.BaseDirectory, "remote-safety.ps1");
        var file = new FileInfo(script);
        if (!file.Exists || file.Attributes.HasFlag(FileAttributes.ReparsePoint)) throw new InvalidDataException("remote_safety_script_missing");
        var acl = file.GetAccessControl();
        if (!acl.AreAccessRulesProtected) throw new InvalidDataException("remote_safety_script_permissions");
        foreach (FileSystemAccessRule rule in acl.GetAccessRules(true, true, typeof(SecurityIdentifier)))
            if (rule.AccessControlType == AccessControlType.Allow &&
                rule.IdentityReference.Value is not ("S-1-5-18" or "S-1-5-32-544") &&
                (rule.FileSystemRights & (FileSystemRights.Write | FileSystemRights.Delete | FileSystemRights.ChangePermissions | FileSystemRights.TakeOwnership)) != 0)
                throw new InvalidDataException("remote_safety_script_writable");
        using var timeout = CancellationTokenSource.CreateLinkedTokenSource(token);
        timeout.CancelAfter(TimeSpan.FromSeconds(45));
        using var child = new TerminalRpc.NativeTerminal(80, 24, sandbox.Home,
            new(RemoteSandbox.ShellPath(), ["-NoLogo", "-NoProfile", "-NonInteractive", "-File", script],
                sandbox.Home, environment, sandbox), pipes: true);
        var errors = Task.Run(async () =>
        {
            var buffer = new byte[4096];
            try { while (await child.Error!.ReadAsync(buffer, timeout.Token) != 0) { } }
            catch (Exception) { }
        });
        var exited = Task.Run(child.Wait);
        try
        {
            await Protocol.Write(child.Input, input, timeout.Token);
            child.Input.Close();
            var result = await Protocol.Read(child.Output, 32768, timeout.Token) ?? throw new EndOfStreamException();
            if (await exited.WaitAsync(timeout.Token) != 0 || result["error"] != null)
                throw new InvalidDataException("remote_safety_probe_failed");
            var evidence = result["remote"] as JsonObject ?? throw new InvalidDataException("remote_safety_evidence_missing");
            evidence["sandbox_sid"] = sandbox.SID;
            evidence["session"] = session;
            // VerifyProcess checks TokenIsAppContainer, exact package SID and
            // zero capabilities while the child is still suspended.
            evidence["token_verified"] = true;
            await Protocol.Write(stream, result, token);
        }
        finally
        {
            child.Stop();
            try { await exited; } catch { }
            await errors;
        }
    }
}
