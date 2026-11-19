using System.ComponentModel;
using System.Runtime.InteropServices;
using System.Security.AccessControl;
using System.Security.Principal;
using System.Text;

namespace Cage.GuestClipboard;

// The headless execution boundary is separate from the interactive GUI guard.
// No internet/private-network capability, inherited handles or user environment
// is given to the workload. The trusted bridge stays outside this container.
internal sealed class RemoteSandbox : IDisposable
{
    private nint sid, capabilities;
    private nint station, desktop;
    private static readonly object StationLock = new();
    internal string SID { get; }
    internal string Home { get; }
    internal string Desktop { get; private set; } = "";

    internal string ToolsDirectory { get; }
    internal bool InstancePackage { get; }

    internal RemoteSandbox(Guid instance, string userSID, bool instancePackage)
    {
        using var identity = WindowsIdentity.GetCurrent();
        if (instance == Guid.Empty || identity.User?.Value != userSID ||
            new WindowsPrincipal(identity).IsInRole(WindowsBuiltInRole.Administrator))
            throw new InvalidDataException("sandbox_requires_bound_standard_user");
        InstancePackage = instancePackage;
        var profile = Environment.GetFolderPath(Environment.SpecialFolder.UserProfile);
        ToolsDirectory = Path.Combine(profile, instancePackage ? @"AppData\Local\Programs\Tools" : @"Cage\CLI\tools");
        var name = instancePackage ? instance.ToString("D") : "Cage.Remote." + instance.ToString("N");
        var result = CreateAppContainerProfile(name, name, "Command environment", 0, 0, out sid);
        if (result == unchecked((int)0x800700B7))
            result = DeriveAppContainerSidFromAppContainerName(name, out sid);
        Marshal.ThrowExceptionForHR(result);
        try
        {
            SID = new SecurityIdentifier(sid).Value;
            Marshal.ThrowExceptionForHR(GetAppContainerFolderPath(SID, out var folder));
            try { Home = Path.Combine(Marshal.PtrToStringUni(folder)!, "LocalState"); }
            finally { Marshal.FreeCoTaskMem(folder); }
            Directory.CreateDirectory(Home);
            AllowPathMetadata(Home);
            capabilities = Marshal.AllocHGlobal(Marshal.SizeOf<SecurityCapabilities>());
            Marshal.StructureToPtr(new SecurityCapabilities { AppContainerSid = sid }, capabilities, false);
            CreateHeadlessDesktop(userSID);
        }
        catch { Dispose(); throw; }
    }

    private void CreateHeadlessDesktop(string userSID)
    {
        // USER32 initializes even for PowerShell/Git. The batch-logon desktop
        // does not admit an AppContainer. Give only this package a private,
        // noninteractive station instead of exposing the logged-on desktop.
        if (System.Diagnostics.Process.GetCurrentProcess().SessionId != 0)
            throw new InvalidDataException("sandbox_requires_headless_session");
        var name = Guid.NewGuid().ToString("N");
        if (!ConvertStringSecurityDescriptorToSecurityDescriptor(
            $"D:P(A;;GA;;;SY)(A;;GA;;;{userSID})(A;;GA;;;{SID})(A;;GA;;;RC)(A;;GA;;;AC)(A;;GA;;;S-1-15-2-2)S:(ML;;NW;;;LW)", 1, out var descriptor, out _))
            throw new Win32Exception();
        try
        {
            var security = new SecurityAttributes { Length = Marshal.SizeOf<SecurityAttributes>(), Descriptor = descriptor };
            lock (StationLock)
            {
                // Standard users cannot name a new station. Open only this
                // batch logon's station, never Winsta0 or another session's.
                station = CreateWindowStation(null, 0, 0xE000A, ref security);
                if (station == 0) throw new Win32Exception();
                var stationName = new StringBuilder(256);
                if (!GetUserObjectInformation(station, 2, stationName, 512, out _)) throw new Win32Exception();
                if (!stationName.ToString().StartsWith("Service-", StringComparison.OrdinalIgnoreCase) ||
                    stationName.ToString().Equals("Service-0x0-3e7$", StringComparison.OrdinalIgnoreCase))
                    throw new InvalidDataException("sandbox_station_invalid");
                uint information = 4;
                GetUserObjectSecurity(station, ref information, null, 0, out var needed);
                var bytes = new byte[needed];
                if (!GetUserObjectSecurity(station, ref information, bytes, needed, out _)) throw new Win32Exception();
                var acl = new RawSecurityDescriptor(bytes, 0);
                acl.DiscretionaryAcl!.InsertAce(0, new CommonAce(AceFlags.None, AceQualifier.AccessAllowed,
                    0x20037F, new SecurityIdentifier(SID), false, null));
                acl.DiscretionaryAcl.InsertAce(0, new CommonAce(AceFlags.None, AceQualifier.AccessAllowed,
                    0xF037F, new SecurityIdentifier(WellKnownSidType.RestrictedCodeSid, null), false, null));
                foreach (var appGroup in new[] { "S-1-15-2-1", "S-1-15-2-2" })
                    acl.DiscretionaryAcl.InsertAce(0, new CommonAce(AceFlags.None, AceQualifier.AccessAllowed,
                        0x20327, new SecurityIdentifier(appGroup), false, null));
                bytes = new byte[acl.BinaryLength]; acl.GetBinaryForm(bytes, 0);
                if (!SetUserObjectSecurity(station, ref information, bytes)) throw new Win32Exception();
                var label = new RawSecurityDescriptor("S:(ML;;NW;;;LW)");
                bytes = new byte[label.BinaryLength]; label.GetBinaryForm(bytes, 0);
                information = 16; // LABEL_SECURITY_INFORMATION, only this batch station.
                if (!SetUserObjectSecurity(station, ref information, bytes)) throw new Win32Exception();
                var previous = GetProcessWindowStation();
                if (!SetProcessWindowStation(station)) throw new Win32Exception();
                try
                {
                    desktop = CreateDesktop(name, null, 0, 0, 0xF01FF, ref security);
                    if (desktop == 0) throw new Win32Exception();
                }
                finally { if (!SetProcessWindowStation(previous)) throw new Win32Exception(); }
                Desktop = stationName + "\\" + name;
            }
        }
        finally { LocalFree(descriptor); }
    }

    internal void Apply(nint attributes)
    {
        if (!UpdateProcThreadAttribute(attributes, 0, 0x00020009, capabilities,
                (nuint)Marshal.SizeOf<SecurityCapabilities>(), 0, 0))
            throw new Win32Exception();
    }

    // Verify the suspended child before it can execute, including exact package
    // identity; an ordinary low-integrity token is not an AppContainer token.
    internal void VerifyProcess(nint process)
    {
        if (!OpenProcessToken(process, 8, out var token)) throw new Win32Exception();
        try
        {
            if (!GetTokenInformation(token, 29, out var isContainer, 4, out _) || isContainer != 1)
                throw new InvalidDataException("sandbox_token_missing");
            var buffer = Marshal.AllocHGlobal(256);
            try
            {
                if (!GetTokenInformationBuffer(token, 31, buffer, 256, out _) ||
                    new SecurityIdentifier(Marshal.ReadIntPtr(buffer)).Value != SID)
                    throw new InvalidDataException("sandbox_token_mismatch");
                if (!GetTokenInformationBuffer(token, 30, buffer, 256, out _) || Marshal.ReadInt32(buffer) != 0)
                    throw new InvalidDataException("sandbox_capabilities_not_empty");
            }
            finally { Marshal.FreeHGlobal(buffer); }
        }
        finally { CloseHandle(token); }
    }

    internal void AllowReadTools()
    {
        // This helper is only for the dedicated user's installed tool tree. Do
        // not grant the package access to pairing data or the whole user profile.
        var path = ToolsDirectory;
        AllowPathMetadata(path);
        for (var directory = new DirectoryInfo(path); directory is not null; directory = directory.Parent)
            if (directory.Attributes.HasFlag(FileAttributes.ReparsePoint)) throw new InvalidDataException("sandbox_tools_redirected");
        var directories = new[] { path }.Concat(Directory.EnumerateDirectories(path, "*", new EnumerationOptions
            { RecurseSubdirectories = true, AttributesToSkip = FileAttributes.ReparsePoint, IgnoreInaccessible = false }));
        foreach (var entry in directories)
        {
            var info = new DirectoryInfo(entry);
            var acl = info.GetAccessControl();
            // Installers deliberately protect each tool root from inheritance.
            if (entry != path && !acl.AreAccessRulesProtected) continue;
            acl.AddAccessRule(new FileSystemAccessRule(new SecurityIdentifier(SID), FileSystemRights.ReadAndExecute,
                InheritanceFlags.ContainerInherit | InheritanceFlags.ObjectInherit, PropagationFlags.None, AccessControlType.Allow));
            info.SetAccessControl(acl);
        }
    }

    private void AllowPathMetadata(string path)
    {
        var profile = Environment.GetFolderPath(Environment.SpecialFolder.UserProfile);
        for (var parent = new DirectoryInfo(path).Parent; parent != null &&
            (parent.FullName.Equals(profile, StringComparison.OrdinalIgnoreCase) ||
             parent.FullName.StartsWith(profile + Path.DirectorySeparatorChar, StringComparison.OrdinalIgnoreCase)); parent = parent.Parent)
        {
            if (parent.Attributes.HasFlag(FileAttributes.ReparsePoint)) throw new InvalidDataException("sandbox_path_redirected");
            var acl = parent.GetAccessControl();
            // GetLongPathName needs directory entry names for path spelling.
            // This non-inherited grant exposes names, not file contents or
            // access to any child outside the explicitly granted home/tools.
            acl.AddAccessRule(new FileSystemAccessRule(new SecurityIdentifier(SID),
                FileSystemRights.ListDirectory | FileSystemRights.Traverse | FileSystemRights.ReadAttributes | FileSystemRights.ReadExtendedAttributes | FileSystemRights.ReadPermissions | FileSystemRights.Synchronize,
                InheritanceFlags.None, PropagationFlags.None, AccessControlType.Allow));
            parent.SetAccessControl(acl);
        }
    }

    internal Dictionary<string, string> EnvironmentBlock()
    {
        var temp = Path.Combine(Home, "Temp"); Directory.CreateDirectory(temp);
        return new(StringComparer.OrdinalIgnoreCase)
        {
            ["SystemRoot"] = Environment.GetFolderPath(Environment.SpecialFolder.Windows),
            ["WINDIR"] = Environment.GetFolderPath(Environment.SpecialFolder.Windows),
            ["SystemDrive"] = Path.GetPathRoot(Environment.SystemDirectory)!.TrimEnd('\\'),
            ["COMSPEC"] = Path.Combine(Environment.SystemDirectory, "cmd.exe"),
            ["PATHEXT"] = ".COM;.EXE;.BAT;.CMD",
            ["PATH"] = Environment.SystemDirectory + ";" + Path.Combine(Environment.SystemDirectory, "WindowsPowerShell", "v1.0"),
            ["USERPROFILE"] = Home, ["HOME"] = Home, ["APPDATA"] = Home, ["LOCALAPPDATA"] = Home,
            ["TEMP"] = temp, ["TMP"] = temp, ["PYTHONUTF8"] = "1", ["PYTHONDONTWRITEBYTECODE"] = "1",
            ["CLAUDE_CODE_USE_POWERSHELL_TOOL"] = "1"
        };
    }

    internal Dictionary<string,string> WorkloadEnvironment()
    {
        var environment = EnvironmentBlock();
        var receipt = Path.Combine(AppContext.BaseDirectory, "cli-tools.json");
        Pairing.PrivateFile(receipt);
        var tools = System.Text.Json.Nodes.JsonNode.Parse(File.ReadAllBytes(receipt))!;
        var toolRoot = ToolsDirectory + Path.DirectorySeparatorChar;
        var paths = new List<string> { Path.GetDirectoryName(ShellPath())! };
        foreach (var name in new[] { "python", "claude", "gh", "git_bash" })
        {
            var executable = tools[name]?.GetValue<string>();
            if (executable is null) continue;
            if (!Path.GetFullPath(executable).StartsWith(toolRoot, StringComparison.OrdinalIgnoreCase))
                throw new InvalidDataException("sandbox_tool_path_invalid");
            var directory = Path.GetDirectoryName(executable)!;
            if (name == "git_bash")
            {
                directory = Path.Combine(Path.GetDirectoryName(directory)!, "cmd");
            }
            paths.Add(directory);
        }
        environment["PATH"] = string.Join(';', paths) + ";" + environment["PATH"];
        var proxyCredential = Path.Combine(AppContext.BaseDirectory, "remote-egress.json");
        Pairing.PrivateFile(proxyCredential);
        var token = System.Text.Json.Nodes.JsonNode.Parse(File.ReadAllBytes(proxyCredential))?["token"]?.GetValue<string>();
        if (token is null || token.Length != 64 || token.Any(c => !char.IsAsciiHexDigitLower(c)))
            throw new InvalidDataException("sandbox_proxy_credential_invalid");
        var proxy = $"http://proxy:{token}@127.0.0.1:{RemoteNetworkPolicy.BrokerPort}";
        environment["HTTP_PROXY"] = proxy; environment["HTTPS_PROXY"] = proxy;
        environment["NODE_USE_ENV_PROXY"] = "1";
        return environment;
    }

    internal static string ShellPath()
    {
        var shell = Path.Combine(AppContext.BaseDirectory, "powershell", "pwsh.exe");
        if (!File.Exists(shell)) throw new InvalidDataException("headless_powershell_missing");
        return shell;
    }

    public void Dispose()
    {
        if (desktop != 0) { CloseDesktop(desktop); desktop = 0; }
        if (station != 0) { CloseWindowStation(station); station = 0; }
        if (capabilities != 0) { Marshal.FreeHGlobal(capabilities); capabilities = 0; }
        if (sid != 0) { FreeSid(sid); sid = 0; }
    }
    [StructLayout(LayoutKind.Sequential)] private struct SecurityCapabilities { internal nint AppContainerSid, Capabilities; internal uint CapabilityCount, Reserved; }
    [StructLayout(LayoutKind.Sequential)] private struct SecurityAttributes { internal int Length; internal nint Descriptor; internal int Inherit; }
    [DllImport("advapi32.dll", CharSet = CharSet.Unicode, SetLastError = true)] private static extern bool ConvertStringSecurityDescriptorToSecurityDescriptor(string text, uint revision, out nint descriptor, out uint size);
    [DllImport("kernel32.dll")] private static extern nint LocalFree(nint pointer);
    [DllImport("user32.dll", CharSet = CharSet.Unicode, SetLastError = true)] private static extern nint CreateWindowStation(string? name, uint flags, uint access, ref SecurityAttributes security);
    [DllImport("user32.dll", CharSet = CharSet.Unicode, SetLastError = true)] private static extern nint CreateDesktop(string name, string? device, nint mode, uint flags, uint access, ref SecurityAttributes security);
    [DllImport("user32.dll", SetLastError = true)] private static extern nint GetProcessWindowStation();
    [DllImport("user32.dll", SetLastError = true)] private static extern bool SetProcessWindowStation(nint station);
    [DllImport("user32.dll", CharSet = CharSet.Unicode, SetLastError = true)] private static extern bool GetUserObjectInformation(nint handle, int index, StringBuilder value, uint bytes, out uint needed);
    [DllImport("user32.dll", SetLastError = true)] private static extern bool GetUserObjectSecurity(nint handle, ref uint information, byte[]? descriptor, uint size, out uint needed);
    [DllImport("user32.dll", SetLastError = true)] private static extern bool SetUserObjectSecurity(nint handle, ref uint information, byte[] descriptor);
    [DllImport("user32.dll")] private static extern bool CloseDesktop(nint desktop);
    [DllImport("user32.dll")] private static extern bool CloseWindowStation(nint station);
    [DllImport("userenv.dll", CharSet = CharSet.Unicode)] private static extern int CreateAppContainerProfile(string name, string display, string description, nint capabilities, uint count, out nint sid);
    [DllImport("userenv.dll", CharSet = CharSet.Unicode)] private static extern int DeriveAppContainerSidFromAppContainerName(string name, out nint sid);
    [DllImport("userenv.dll", CharSet = CharSet.Unicode)] private static extern int GetAppContainerFolderPath(string sid, out nint path);
    [DllImport("advapi32.dll")] private static extern nint FreeSid(nint sid);
    [DllImport("advapi32.dll", SetLastError = true)] [return: MarshalAs(UnmanagedType.Bool)] private static extern bool OpenProcessToken(nint process, uint access, out nint token);
    [DllImport("advapi32.dll", SetLastError = true)] [return: MarshalAs(UnmanagedType.Bool)] private static extern bool GetTokenInformation(nint token, int kind, out int value, uint length, out uint returned);
    [DllImport("advapi32.dll", EntryPoint = "GetTokenInformation", SetLastError = true)] [return: MarshalAs(UnmanagedType.Bool)] private static extern bool GetTokenInformationBuffer(nint token, int kind, nint value, uint length, out uint returned);
    [DllImport("kernel32.dll", SetLastError = true)] [return: MarshalAs(UnmanagedType.Bool)] private static extern bool UpdateProcThreadAttribute(nint list, uint flags, nuint attribute, nint value, nuint size, nint previous, nint returned);
    [DllImport("kernel32.dll")] private static extern bool CloseHandle(nint handle);
}
