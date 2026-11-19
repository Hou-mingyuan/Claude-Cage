using System.ComponentModel;
using System.Runtime.InteropServices;
using System.Security.AccessControl;
using System.Security.Principal;
using System.Text.Json;
using Microsoft.Win32.SafeHandles;

namespace Cage.GuestClipboard;

// A scoped grant for the system null sink; no ordinary file, named-object
// namespace, network capability or other account's permissions are changed.
internal static class RemoteSandboxPolicy
{
    internal static void Run(string action, string profilePath)
    {
        var profile = WindowsProfile.Load(profilePath);
        using var caller = WindowsIdentity.GetCurrent();
        if (action is not ("status" or "install") || !profile.Remote ||
            !new WindowsPrincipal(caller).IsInRole(WindowsBuiltInRole.Administrator))
            throw new InvalidDataException("sandbox_policy_request_invalid");
        Marshal.ThrowExceptionForHR(DeriveAppContainerSidFromAppContainerName(profile.PackageName, out var sid));
        try
        {
            var package = new SecurityIdentifier(sid);
            var bootstrap = new FileInfo(Path.Combine(AppContext.BaseDirectory, "remote-shell.ps1"));
            if (action == "install")
            {
                if (bootstrap.Attributes.HasFlag(FileAttributes.ReparsePoint)) throw new InvalidDataException("sandbox_shell_redirected");
                var security = bootstrap.GetAccessControl();
                security.AddAccessRule(new FileSystemAccessRule(package, FileSystemRights.ReadAndExecute, AccessControlType.Allow));
                bootstrap.SetAccessControl(security);
                var python = new DirectoryInfo(Path.Combine(AppContext.BaseDirectory, "remote-python"));
                if (python.Exists)
                {
                    if (python.Attributes.HasFlag(FileAttributes.ReparsePoint)) throw new InvalidDataException("sandbox_python_redirected");
                    var pythonACL = python.GetAccessControl();
                    pythonACL.AddAccessRule(new FileSystemAccessRule(package, FileSystemRights.ReadAndExecute,
                        InheritanceFlags.ContainerInherit | InheritanceFlags.ObjectInherit, PropagationFlags.None, AccessControlType.Allow));
                    python.SetAccessControl(pythonACL);
                }
                var probe = new FileInfo(Path.Combine(AppContext.BaseDirectory, "remote-safety.ps1"));
                if (probe.Exists)
                {
                    if (probe.Attributes.HasFlag(FileAttributes.ReparsePoint)) throw new InvalidDataException("sandbox_probe_redirected");
                    var code = new FileSecurity();
                    code.SetAccessRuleProtection(true, false);
                    foreach (var principal in new[] { "S-1-5-18", "S-1-5-32-544" })
                        code.AddAccessRule(new FileSystemAccessRule(new SecurityIdentifier(principal), FileSystemRights.FullControl, AccessControlType.Allow));
                    foreach (var principal in new[] { profile.SID, package.Value })
                        code.AddAccessRule(new FileSystemAccessRule(new SecurityIdentifier(principal), FileSystemRights.ReadAndExecute, AccessControlType.Allow));
                    probe.SetAccessControl(code);
                }
            }
            // .NET resolves every path component, including C:\Users. Grant
            // metadata/entry names on those ancestors only, never inherited
            // file access. The ordinary bridge handles its own profile tree.
            var users = Directory.GetParent(Environment.GetFolderPath(Environment.SpecialFolder.UserProfile))!;
            var metadata = FileSystemRights.ListDirectory | FileSystemRights.Traverse | FileSystemRights.ReadAttributes |
                FileSystemRights.ReadExtendedAttributes | FileSystemRights.ReadPermissions | FileSystemRights.Synchronize;
            var ancestorsReady = true;
            for (var parent = users; parent != null; parent = parent.Parent)
            {
                if (parent.Attributes.HasFlag(FileAttributes.ReparsePoint)) throw new InvalidDataException("sandbox_ancestor_redirected");
                var security = parent.GetAccessControl();
                var present = security.GetAccessRules(true, false, typeof(SecurityIdentifier)).OfType<FileSystemAccessRule>()
                    .Any(rule => rule.IdentityReference.Equals(package) && rule.AccessControlType == AccessControlType.Allow &&
                        rule.InheritanceFlags == InheritanceFlags.None && (rule.FileSystemRights & metadata) == metadata);
                if (action == "install" && !present)
                {
                    security.AddAccessRule(new FileSystemAccessRule(package, metadata, InheritanceFlags.None, PropagationFlags.None, AccessControlType.Allow));
                    parent.SetAccessControl(security);
                    present = true;
                }
                ancestorsReady &= present;
            }
            using var file = CreateFile(@"\\.\NUL", action == "install" ? 0x60000u : 0x20000u, 3, 0, 3, 0, 0);
            if (file.IsInvalid) throw new Win32Exception();
            Check(GetSecurityInfo(file, 1, 4, out _, out _, out _, out _, out var descriptor));
            RawSecurityDescriptor acl;
            try
            {
                var bytes = new byte[GetSecurityDescriptorLength(descriptor)];
                Marshal.Copy(descriptor, bytes, 0, bytes.Length);
                acl = new RawSecurityDescriptor(bytes, 0);
            }
            finally { LocalFree(descriptor); }
            if (acl.DiscretionaryAcl is null) throw new InvalidDataException("sandbox_null_acl_missing");
            var granted = acl.DiscretionaryAcl.OfType<CommonAce>().Any(ace => ace.AceQualifier == AceQualifier.AccessAllowed &&
                ace.SecurityIdentifier.Equals(package) && (ace.AccessMask & 0x12019F) == 0x12019F);
            if (action == "install" && !granted)
            {
                acl.DiscretionaryAcl.InsertAce(0, new CommonAce(AceFlags.None, AceQualifier.AccessAllowed, 0x12019F, package, false, null));
                var bytes = new byte[acl.DiscretionaryAcl.BinaryLength]; acl.DiscretionaryAcl.GetBinaryForm(bytes, 0);
                var pointer = Marshal.AllocHGlobal(bytes.Length);
                try { Marshal.Copy(bytes, 0, pointer, bytes.Length); Check(SetSecurityInfo(file, 1, 4, 0, 0, pointer, 0)); }
                finally { Marshal.FreeHGlobal(pointer); }
                granted = true;
            }
            Console.WriteLine(JsonSerializer.Serialize(new { version = 1, null_sink = granted, path_metadata = ancestorsReady, package = package.Value,
                descriptor = acl.GetSddlForm(AccessControlSections.Access), workload_ready = false }));
        }
        finally { FreeSid(sid); }
    }
    private static void Check(uint result) { if (result != 0) throw new Win32Exception((int)result); }
    [DllImport("userenv.dll", CharSet = CharSet.Unicode)] private static extern int DeriveAppContainerSidFromAppContainerName(string name, out nint sid);
    [DllImport("advapi32.dll")] private static extern nint FreeSid(nint sid);
    [DllImport("kernel32.dll", CharSet = CharSet.Unicode, SetLastError = true)] private static extern SafeFileHandle CreateFile(string name, uint access, uint share, nint security, uint creation, uint flags, nint template);
    [DllImport("advapi32.dll")] private static extern uint GetSecurityInfo(SafeFileHandle handle, int kind, uint information, out nint owner, out nint group, out nint dacl, out nint sacl, out nint descriptor);
    [DllImport("advapi32.dll")] private static extern uint SetSecurityInfo(SafeFileHandle handle, int kind, uint information, nint owner, nint group, nint dacl, nint sacl);
    [DllImport("advapi32.dll")] private static extern uint GetSecurityDescriptorLength(nint descriptor);
    [DllImport("kernel32.dll")] private static extern nint LocalFree(nint pointer);
}
