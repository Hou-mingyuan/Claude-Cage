using System.IO.Pipes;
using System.Security.AccessControl;
using System.Security.Principal;
using Microsoft.Win32;

namespace Cage.GuestClipboard;

// Local transport for a controller and a limited interactive user on the same
// dedicated cloud Windows machine. SSH remains the only remote entry point.
internal static class LocalGuestTransport
{
    internal static void VerifyMachine(Guid expected)
    {
        using var registry = RegistryKey.OpenBaseKey(RegistryHive.LocalMachine, RegistryView.Registry64);
        using var key = registry.OpenSubKey(@"SOFTWARE\Microsoft\Cryptography");
        if (expected == Guid.Empty || !Guid.TryParse(key?.GetValue("MachineGuid") as string, out var actual) || actual != expected)
            throw new InvalidOperationException("remote_machine_identity_mismatch");
    }

    static string Name(Guid instance, string sid, bool headless) => "Cage.Remote." + instance.ToString("N") + "." + sid + (headless ? ".Headless" : "");

    internal static NamedPipeServerStream Listen(Guid instance, string sid, bool headless = false)
    {
        var acl = new PipeSecurity();
        acl.SetAccessRuleProtection(true, false);
        acl.SetOwner(new SecurityIdentifier(sid));
        // Explicitly reject network logons, including administrators over SMB.
        acl.AddAccessRule(new PipeAccessRule(new SecurityIdentifier(WellKnownSidType.NetworkSid, null), PipeAccessRights.FullControl, AccessControlType.Deny));
        foreach (var owner in new[] { sid, "S-1-5-18", "S-1-5-32-544" })
            acl.AddAccessRule(new PipeAccessRule(new SecurityIdentifier(owner), PipeAccessRights.FullControl, AccessControlType.Allow));
        return NamedPipeServerStreamAcl.Create(Name(instance, sid, headless), PipeDirection.InOut, 64,
            PipeTransmissionMode.Byte, PipeOptions.Asynchronous, 64 << 10, 64 << 10, acl);
    }

    internal static async Task<Stream> Connect(Guid instance, string sid, CancellationToken token, bool headless = false)
    {
        // Identification prevents the unprivileged GUI server from impersonating
        // the administrative controller. HMAC still authenticates both endpoints.
        var pipe = new NamedPipeClientStream(".", Name(instance, sid, headless), PipeDirection.InOut,
            PipeOptions.Asynchronous, TokenImpersonationLevel.Identification);
        try { await pipe.ConnectAsync(token); return pipe; }
        catch { pipe.Dispose(); throw; }
    }
}
