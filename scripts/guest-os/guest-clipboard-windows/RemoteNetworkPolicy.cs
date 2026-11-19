using System.ComponentModel;
using System.Runtime.InteropServices;
using System.Security.AccessControl;
using System.Security.Cryptography;
using System.Security.Principal;
using System.Text;
using System.Text.Json;

namespace Cage.GuestClipboard;

// A persistent, account-scoped offline baseline for a remote Windows host.
// WFP enforcement does not depend on the Defender Firewall profile switches.
// This deliberately does NOT certify the full remote boundary: service-brokered
// DNS/HTTP and the controlled proxy need their own integration before workloads
// can be admitted. In particular, never substitute this for PacketReady().
internal static unsafe partial class RemoteNetworkPolicy
{
    private const uint Persistent = 1, Block = 0x1001, NotFound = 0x80320003;
    private static readonly Guid UserID = new("af043a0a-b34d-4f86-979c-c90371af6e66");
    private static readonly (string Name, Guid Layer)[] Layers =
    [
        ("connect-v4", new("c38d57d1-05a7-4c33-904f-7fbceee60e82")),
        ("connect-v6", new("4a72393b-319f-44bc-84c3-ba54dcb3b6b4")),
        ("receive-v4", new("e1cd9fe7-f4b5-4273-96c0-592e487b8650")),
        ("receive-v6", new("a3b42c97-9f04-4672-b87e-cee9c483257f")),
    ];

    internal static void Run(string action, string profilePath)
    {
        if (action is not ("install" or "status" or "remove" or "broker-install" or "broker-status" or "broker-remove")) throw new InvalidDataException("network_policy_action_invalid");
        var profile = WindowsProfile.Load(profilePath);
        using var caller = WindowsIdentity.GetCurrent();
        if (!profile.Remote || string.IsNullOrEmpty(profile.User) ||
            !new WindowsPrincipal(caller).IsInRole(WindowsBuiltInRole.Administrator) ||
            caller.User?.Value == profile.SID || profile.SID.EndsWith("-500", StringComparison.Ordinal) ||
            new NTAccount(Environment.MachineName, profile.User).Translate(typeof(SecurityIdentifier)).Value != profile.SID)
            throw new InvalidDataException("network_policy_account_invalid");
        Check(NetUserGetLocalGroups(null, profile.User, 0, 1, out var groups, uint.MaxValue, out var count, out _));
        try
        {
            for (var i = 0; i < count; i++)
            {
                var name = Marshal.PtrToStringUni(Marshal.ReadIntPtr(groups, checked((int)i * IntPtr.Size)));
                if (string.IsNullOrEmpty(name) || new NTAccount(name).Translate(typeof(SecurityIdentifier)).Value == "S-1-5-32-544")
                    throw new InvalidDataException("network_policy_requires_standard_account");
            }
        }
        finally { if (groups != IntPtr.Zero) NetApiBufferFree(groups); }

        if (action.StartsWith("broker-", StringComparison.Ordinal)) { Broker(action[7..], profile); return; }

        Check(FwpmEngineOpen0(null, 10, IntPtr.Zero, IntPtr.Zero, out var engine));
        try
        {
            var sublayerKey = Key(profile, "sublayer");
            var descriptor = new RawSecurityDescriptor($"D:(A;;CC;;;{profile.SID})");
            var bytes = new byte[descriptor.BinaryLength];
            descriptor.GetBinaryForm(bytes, 0);
            fixed (byte* data = bytes)
            {
                var blob = new Blob { Size = (uint)bytes.Length, Data = (IntPtr)data };
                var condition = new Condition { Field = UserID, Value = new Value { Type = 14, Data = (ulong)&blob } };
                if (action is "install" or "remove")
                {
                    Check(FwpmTransactionBegin0(engine, 0));
                    var committed = false;
                    try
                    {
                        if (action == "install")
                        {
                            var layer = new Sublayer { Key = sublayerKey, Flags = Persistent, Weight = 0x8000,
                                Display = new Display { Name = $"Cage remote account {profile.VM:D}" } };
                            var result = FwpmSubLayerAdd0(engine, ref layer, IntPtr.Zero);
                            if (result != 0x80320009) Check(result); // FWP_E_ALREADY_EXISTS
                        }
                        foreach (var entry in Layers)
                        {
                            var key = Key(profile, entry.Name);
                            var result = FwpmFilterGetByKey0(engine, ref key, out var existing);
                            if (result == 0)
                            {
                                try
                                {
                                    // Never silently replace another policy, even at our key.
                                    if (!Matches(existing, key, sublayerKey, entry.Layer, bytes))
                                        throw new InvalidDataException("network_policy_conflict");
                                }
                                finally { FwpmFreeMemory0(ref existing); }
                                if (action == "remove") Check(FwpmFilterDeleteByKey0(engine, ref key));
                            }
                            else if (result == NotFound)
                            {
                                if (action == "install")
                                {
                                    var filter = new Filter { Key = key, Flags = Persistent, Layer = entry.Layer, Sublayer = sublayerKey,
                                        Display = new Display { Name = $"Cage remote {profile.VM:D} {entry.Name}" },
                                        Count = 1, Conditions = (IntPtr)(&condition), Action = new Action { Type = Block } };
                                    Check(FwpmFilterAdd0(engine, ref filter, IntPtr.Zero, out _));
                                }
                            }
                            else Check(result);
                        }
                        if (action == "remove")
                        {
                            var result = FwpmSubLayerDeleteByKey0(engine, ref sublayerKey);
                            if (result != 0x80320007) Check(result); // FWP_E_SUBLAYER_NOT_FOUND
                        }
                        Check(FwpmTransactionCommit0(engine));
                        committed = true;
                    }
                    finally { if (!committed) FwpmTransactionAbort0(engine); }
                }
                var installed = 0;
                foreach (var entry in Layers)
                {
                    var key = Key(profile, entry.Name);
                    var result = FwpmFilterGetByKey0(engine, ref key, out var current);
                    if (result == NotFound) continue;
                    Check(result);
                    try { if (Matches(current, key, sublayerKey, entry.Layer, bytes)) installed++; }
                    finally { FwpmFreeMemory0(ref current); }
                }
                Console.WriteLine(JsonSerializer.Serialize(new { version = 1, instance = profile.VM, gui_user_sid = profile.SID,
                    policy = "offline-direct-sockets", installed_filters = installed, required_filters = Layers.Length,
                    direct_socket_filters_installed = installed == Layers.Length, workload_ready = false }));
            }
        }
        finally { FwpmEngineClose0(engine); }
    }

    private static Guid Key(WindowsProfile profile, string name) => new(SHA256.HashData(
        Encoding.UTF8.GetBytes($"Cage.Remote.Network/v1\0{profile.VM:D}\0{profile.SID}\0{name}")).AsSpan(0, 16));

    private static bool Matches(IntPtr pointer, Guid key, Guid sublayer, Guid layer, byte[] descriptor)
    {
        var filter = Marshal.PtrToStructure<Filter>(pointer);
        if (filter.Key != key || filter.Sublayer != sublayer || filter.Layer != layer || filter.Flags != Persistent ||
            filter.Action.Type != Block || filter.Count != 1 || filter.Conditions == IntPtr.Zero) return false;
        var condition = Marshal.PtrToStructure<Condition>(filter.Conditions);
        if (condition.Field != UserID || condition.Match != 0 || condition.Value.Type != 14 || condition.Value.Data == 0) return false;
        var blob = Marshal.PtrToStructure<Blob>((IntPtr)condition.Value.Data);
        if (blob.Size != descriptor.Length || blob.Data == IntPtr.Zero) return false;
        return new ReadOnlySpan<byte>((void*)blob.Data, (int)blob.Size).SequenceEqual(descriptor);
    }
    private static void Check(uint result) { if (result != 0) throw new Win32Exception(unchecked((int)result)); }

    // Native layouts from Windows SDK fwpmtypes.h/fwptypes.h; this bridge is x64.
    [StructLayout(LayoutKind.Sequential, CharSet = CharSet.Unicode)]
    private struct Display { [MarshalAs(UnmanagedType.LPWStr)] public string? Name; [MarshalAs(UnmanagedType.LPWStr)] public string? Description; }
    [StructLayout(LayoutKind.Sequential)] private struct Blob { public uint Size; public IntPtr Data; }
    [StructLayout(LayoutKind.Sequential)] private struct Value { public uint Type; public ulong Data; }
    [StructLayout(LayoutKind.Sequential)] private struct Condition { public Guid Field; public uint Match; public Value Value; }
    [StructLayout(LayoutKind.Sequential)] private struct Action { public uint Type; public Guid Key; }
    [StructLayout(LayoutKind.Explicit, Size = 16)] private struct Context { [FieldOffset(0)] public ulong Raw; [FieldOffset(0)] public Guid Key; }
    [StructLayout(LayoutKind.Sequential)] private struct Sublayer
    { public Guid Key; public Display Display; public uint Flags; public IntPtr Provider; public Blob ProviderData; public ushort Weight; }
    [StructLayout(LayoutKind.Sequential)] private struct Filter
    {
        public Guid Key; public Display Display; public uint Flags; public IntPtr Provider; public Blob ProviderData;
        public Guid Layer, Sublayer; public Value Weight; public uint Count; public IntPtr Conditions;
        public Action Action; public Context Context; public IntPtr Reserved; public ulong ID; public Value EffectiveWeight;
    }
    [DllImport("fwpuclnt.dll", CharSet = CharSet.Unicode)] private static extern uint FwpmEngineOpen0(string? server, uint auth, IntPtr identity, IntPtr session, out IntPtr engine);
    [DllImport("fwpuclnt.dll")] private static extern uint FwpmEngineClose0(IntPtr engine);
    [DllImport("fwpuclnt.dll")] private static extern uint FwpmTransactionBegin0(IntPtr engine, uint flags);
    [DllImport("fwpuclnt.dll")] private static extern uint FwpmTransactionCommit0(IntPtr engine);
    [DllImport("fwpuclnt.dll")] private static extern uint FwpmTransactionAbort0(IntPtr engine);
    [DllImport("fwpuclnt.dll")] private static extern uint FwpmSubLayerAdd0(IntPtr engine, ref Sublayer layer, IntPtr security);
    [DllImport("fwpuclnt.dll")] private static extern uint FwpmSubLayerDeleteByKey0(IntPtr engine, ref Guid key);
    [DllImport("fwpuclnt.dll")] private static extern uint FwpmFilterAdd0(IntPtr engine, ref Filter filter, IntPtr security, out ulong id);
    [DllImport("fwpuclnt.dll")] private static extern uint FwpmFilterGetByKey0(IntPtr engine, ref Guid key, out IntPtr filter);
    [DllImport("fwpuclnt.dll")] private static extern uint FwpmFilterDeleteByKey0(IntPtr engine, ref Guid key);
    [DllImport("fwpuclnt.dll")] private static extern void FwpmFreeMemory0(ref IntPtr memory);
    [DllImport("netapi32.dll", CharSet = CharSet.Unicode)] private static extern uint NetUserGetLocalGroups(string? server, string user, uint level, uint flags, out IntPtr groups, uint length, out uint count, out uint total);
    [DllImport("netapi32.dll")] private static extern uint NetApiBufferFree(IntPtr memory);
}
