using System.Runtime.InteropServices;
using System.Security.AccessControl;
using System.Security.Principal;
using System.Text.Json;

namespace Cage.GuestClipboard;

internal static unsafe partial class RemoteNetworkPolicy
{
    internal const int BrokerPort = 17897;
    private static readonly Guid PackageID = new("71bc78fa-f17c-4997-a602-6abb261f351c");
    private static readonly Guid RemoteAddress = new("b235ae9a-1d64-49b8-a44c-5ff3d9095045");
    private static readonly Guid RemotePort = new("c35a604d-d22b-4e1a-91b4-68f674ee674b");
    private static readonly Guid Protocol = new("3971ef2b-623e-4f9a-8cb1-6e79b806b9a7");

    // Candidate remote deployment only. Keep all four account deny filters,
    // and add package-specific denies: ALE_USER_ID performs an access check,
    // so a user-only descriptor does not match a restricted AppContainer token.
    private static void Broker(string action, WindowsProfile profile)
    {
        Marshal.ThrowExceptionForHR(DeriveAppContainerSidFromAppContainerName(profile.PackageName, out var package));
        try
        {
            if (action == "remove") ConfigureLoopback("remove", package);
            Check(FwpmEngineOpen0(null, 10, IntPtr.Zero, IntPtr.Zero, out var engine));
            try
            {
                var descriptor = new RawSecurityDescriptor($"D:(A;;CC;;;{profile.SID})");
                var bytes = new byte[descriptor.BinaryLength]; descriptor.GetBinaryForm(bytes, 0);
                var key = Key(profile, "broker-connect-v4");
                var sublayer = Key(profile, "sublayer");
                foreach (var entry in Layers)
                {
                    var baseline = Key(profile, entry.Name);
                    Check(FwpmFilterGetByKey0(engine, ref baseline, out var original));
                    try { if (!Matches(original, baseline, sublayer, entry.Layer, bytes)) throw new InvalidDataException("broker_requires_offline_baseline"); }
                    finally { FwpmFreeMemory0(ref original); }
                }
                descriptor = new RawSecurityDescriptor($"D:(A;;CC;;;{profile.SID})(A;;CC;;;{new SecurityIdentifier(package).Value})");
                bytes = new byte[descriptor.BinaryLength]; descriptor.GetBinaryForm(bytes, 0);
                fixed (byte* data = bytes)
                {
                    var blob = new Blob { Size = (uint)bytes.Length, Data = (IntPtr)data };
                    Condition* conditions = stackalloc Condition[5];
                    conditions[0] = new() { Field = UserID, Value = new() { Type = 14, Data = (ulong)&blob } };
                    conditions[1] = new() { Field = PackageID, Value = new() { Type = 13, Data = (ulong)package } };
                    conditions[2] = new() { Field = RemoteAddress, Value = new() { Type = 3, Data = 0x7F000001 } };
                    conditions[3] = new() { Field = RemotePort, Value = new() { Type = 2, Data = BrokerPort } };
                    conditions[4] = new() { Field = Protocol, Value = new() { Type = 1, Data = 6 } };
                    ulong weight = ulong.MaxValue;
                    var expected = new Filter { Key = key, Flags = Persistent, Layer = Layers[0].Layer, Sublayer = sublayer,
                        Display = new() { Name = $"Cage remote {profile.VM:D} sandbox broker" },
                        Weight = new() { Type = 4, Data = (ulong)&weight }, Count = 5, Conditions = (IntPtr)conditions,
                        Action = new() { Type = 0x1002 } }; // Soft permit; other providers may still deny.
                    Check(FwpmTransactionBegin0(engine, 0));
                    var committed = false;
                    bool installed;
                    try
                    {
                        PackageBlocks(engine, action, profile, package);
                        PackageDatagramBindings(engine, action, profile, package);
                        var result = FwpmFilterGetByKey0(engine, ref key, out var current);
                        installed = result == 0;
                        if (installed)
                        {
                            try { if (!BrokerMatches(current, expected, bytes, package)) throw new InvalidDataException("broker_policy_conflict"); }
                            finally { FwpmFreeMemory0(ref current); }
                        }
                        else if (result != NotFound) Check(result);
                        if (action == "install" && !installed) { Check(FwpmFilterAdd0(engine, ref expected, IntPtr.Zero, out _)); installed = true; }
                        if (action == "remove" && installed) { Check(FwpmFilterDeleteByKey0(engine, ref key)); installed = false; }
                        Check(FwpmTransactionCommit0(engine)); committed = true;
                    }
                    finally { if (!committed) FwpmTransactionAbort0(engine); }
                    // WFP's account block remains even while the exemption is changed.
                    var loopback = ConfigureLoopback(action, package);
                    Console.WriteLine(JsonSerializer.Serialize(new { version = 1, broker = "127.0.0.1:" + BrokerPort,
                        package = new SecurityIdentifier(package).Value, broker_filter = installed,
                        loopback_exemption = loopback, udp_bind_filters = 2, deployment_candidate = true, workload_ready = false }));
                }
            }
            finally { FwpmEngineClose0(engine); }
        }
        finally { FreeSid(package); }
    }

    private static void PackageBlocks(IntPtr engine, string action, WindowsProfile profile, IntPtr package)
    {
        var condition = new Condition { Field = PackageID, Value = new() { Type = 13, Data = (ulong)package } };
        ulong weight = ulong.MaxValue - 1; // Below the single broker permit.
        foreach (var entry in Layers)
        {
            var key = Key(profile, "sandbox-" + entry.Name);
            var filter = new Filter { Key = key, Layer = entry.Layer, Sublayer = Key(profile, "sublayer"), Flags = Persistent,
                Display = new() { Name = $"Cage remote {profile.VM:D} sandbox {entry.Name}" },
                Count = 1, Conditions = (IntPtr)(&condition), Action = new() { Type = Block },
                Weight = new() { Type = 4, Data = (ulong)&weight } };
            var result = FwpmFilterGetByKey0(engine, ref key, out var pointer);
            if (result == 0)
            {
                try
                {
                    var existing = Marshal.PtrToStructure<Filter>(pointer);
                    if (existing.Key != key || existing.Sublayer != filter.Sublayer || existing.Layer != filter.Layer ||
                        (existing.Flags & ~0x40u) != Persistent || existing.Action.Type != Block || existing.Count != 1 ||
                        existing.Conditions == IntPtr.Zero || existing.Weight.Type != 4 || existing.Weight.Data == 0 ||
                        *(ulong*)existing.Weight.Data != weight) throw new InvalidDataException("sandbox_network_policy_conflict");
                    var match = *(Condition*)existing.Conditions;
                    if (match.Field != PackageID || match.Match != 0 || match.Value.Type != 13 || match.Value.Data == 0 ||
                        !new SecurityIdentifier((IntPtr)match.Value.Data).Equals(new SecurityIdentifier(package)))
                        throw new InvalidDataException("sandbox_network_policy_conflict");
                }
                finally { FwpmFreeMemory0(ref pointer); }
                // Removing the broker must leave the sandbox offline. Keep
                // its deny filters even after revoking the loopback exception.
            }
            else if (result == NotFound)
            {
                if (action == "install") Check(FwpmFilterAdd0(engine, ref filter, IntPtr.Zero, out _));
                else if (action == "status") throw new InvalidDataException("sandbox_network_policy_missing");
            }
            else Check(result);
        }
    }

    private static bool BrokerMatches(IntPtr pointer, Filter expected, byte[] descriptor, IntPtr package)
    {
        var actual = Marshal.PtrToStructure<Filter>(pointer);
        if (actual.Key != expected.Key || actual.Sublayer != expected.Sublayer || actual.Layer != expected.Layer ||
            (actual.Flags & ~0x40u) != Persistent || actual.Action.Type != expected.Action.Type || actual.Count != 5 ||
            actual.Conditions == IntPtr.Zero || actual.Weight.Type != 4 || actual.Weight.Data == 0 ||
            *(ulong*)actual.Weight.Data != ulong.MaxValue)
            throw new InvalidDataException($"broker_filter_shape_flags={actual.Flags}_action={actual.Action.Type}_count={actual.Count}_weight_type={actual.Weight.Type}_weight={(actual.Weight.Type == 4 && actual.Weight.Data != 0 ? *(ulong*)actual.Weight.Data : actual.Weight.Data)}");
        for (int i = 0; i < 5; i++)
        {
            var a = ((Condition*)actual.Conditions)[i]; var e = ((Condition*)expected.Conditions)[i];
            if (a.Field != e.Field || a.Match != 0 || a.Value.Type != e.Value.Type)
                throw new InvalidDataException($"broker_condition_shape_{i}_{a.Field}_{a.Value.Type}");
            if (a.Field == UserID)
            {
                if (a.Value.Data == 0) return false;
                var blob = *(Blob*)a.Value.Data;
                if (blob.Size != descriptor.Length || blob.Data == IntPtr.Zero ||
                    !new ReadOnlySpan<byte>((void*)blob.Data, (int)blob.Size).SequenceEqual(descriptor))
                    throw new InvalidDataException("broker_user_descriptor_changed");
            }
            else if (a.Field == PackageID)
            {
                if (a.Value.Data == 0 || !new SecurityIdentifier((IntPtr)a.Value.Data).Equals(new SecurityIdentifier(package))) return false;
            }
            else if (a.Value.Data != e.Value.Data) throw new InvalidDataException($"broker_scalar_changed_{i}_{a.Value.Data}_{e.Value.Data}");
        }
        return true;
    }

    // Block implicit and explicit UDP binds before a datagram can be queued.
    // ALE connect drops can look like a successful asynchronous UDP send;
    // this scoped rule also makes the actual rejection visible to the caller.
    // TCP (including the only broker permit) and every other package are untouched.
    private static void PackageDatagramBindings(IntPtr engine, string action, WindowsProfile profile, IntPtr package)
    {
        Condition* conditions = stackalloc Condition[2];
        conditions[0] = new() { Field = PackageID, Value = new() { Type = 13, Data = (ulong)package } };
        conditions[1] = new() { Field = Protocol, Value = new() { Type = 1, Data = 17 } };
        ulong weight = ulong.MaxValue;
        foreach (var (name, layer) in new[] {
            ("udp-bind-v4", new Guid("1247d66d-0b60-4a15-8d44-7155d0f53a0c")),
            ("udp-bind-v6", new Guid("55a650e1-5f0a-4eca-a653-88f53b26aa8c")) })
        {
            var key = Key(profile, name);
            var filter = new Filter { Key = key, Layer = layer, Sublayer = Key(profile, "sublayer"), Flags = Persistent,
                Display = new() { Name = $"Cage remote {profile.VM:D} sandbox {name}" },
                Count = 2, Conditions = (IntPtr)conditions, Action = new() { Type = Block },
                Weight = new() { Type = 4, Data = (ulong)&weight } };
            var found = FwpmFilterGetByKey0(engine, ref key, out var pointer);
            if (found == 0)
            {
                try
                {
                    var actual = Marshal.PtrToStructure<Filter>(pointer);
                    if (actual.Key != key || actual.Layer != layer || actual.Sublayer != filter.Sublayer ||
                        (actual.Flags & ~0x40u) != Persistent || actual.Action.Type != Block || actual.Count != 2 ||
                        actual.Conditions == IntPtr.Zero || actual.Weight.Type != 4 || actual.Weight.Data == 0 ||
                        *(ulong*)actual.Weight.Data != weight) throw new InvalidDataException("sandbox_udp_bind_policy_conflict");
                    for (int i = 0; i < 2; i++)
                    {
                        var value = ((Condition*)actual.Conditions)[i];
                        if (value.Field != conditions[i].Field || value.Match != 0 || value.Value.Type != conditions[i].Value.Type ||
                            (i == 0 ? value.Value.Data == 0 || !new SecurityIdentifier((IntPtr)value.Value.Data).Equals(new SecurityIdentifier(package)) : value.Value.Data != 17))
                            throw new InvalidDataException("sandbox_udp_bind_policy_conflict");
                    }
                }
                finally { FwpmFreeMemory0(ref pointer); }
            }
            else if (found == NotFound)
            {
                if (action == "install") Check(FwpmFilterAdd0(engine, ref filter, IntPtr.Zero, out _));
                else throw new InvalidDataException("sandbox_udp_bind_policy_missing");
            }
            else Check(found);
            // As with the other package blocks, broker removal leaves UDP denied.
        }
    }

    private static bool ConfigureLoopback(string action, IntPtr package)
    {
        Check(NetworkIsolationGetAppContainerConfig(out var count, out var pointer));
        var original = new List<SidAttributes>();
        try
        {
            for (int i = 0; i < count; i++) original.Add(((SidAttributes*)pointer)[i]);
            var wanted = new SecurityIdentifier(package);
            var present = original.Any(entry => new SecurityIdentifier(entry.SID).Equals(wanted));
            var updated = new List<SidAttributes>(original);
            if (action == "install" && !present) updated.Add(new() { SID = package });
            if (action == "remove" && present) updated.RemoveAll(entry => new SecurityIdentifier(entry.SID).Equals(wanted));
            if (updated.Count != original.Count)
            {
                var entries = updated.ToArray();
                fixed (SidAttributes* data = entries) Check(NetworkIsolationSetAppContainerConfig((uint)entries.Length, (IntPtr)data));
            }
            return action == "install" || (action != "remove" && present);
        }
        finally
        {
            foreach (var entry in original) HeapFree(GetProcessHeap(), 0, entry.SID);
            if (pointer != IntPtr.Zero) HeapFree(GetProcessHeap(), 0, pointer);
        }
    }
    [StructLayout(LayoutKind.Sequential)] private struct SidAttributes { public IntPtr SID; public uint Attributes; }
    [DllImport("userenv.dll", CharSet = CharSet.Unicode)] private static extern int DeriveAppContainerSidFromAppContainerName(string name, out IntPtr sid);
    [DllImport("advapi32.dll")] private static extern IntPtr FreeSid(IntPtr sid);
    [DllImport("FirewallAPI.dll")] private static extern uint NetworkIsolationGetAppContainerConfig(out uint count, out IntPtr entries);
    [DllImport("FirewallAPI.dll")] private static extern uint NetworkIsolationSetAppContainerConfig(uint count, IntPtr entries);
    [DllImport("kernel32.dll")] private static extern IntPtr GetProcessHeap();
    [DllImport("kernel32.dll")] private static extern bool HeapFree(IntPtr heap, uint flags, IntPtr memory);
}
