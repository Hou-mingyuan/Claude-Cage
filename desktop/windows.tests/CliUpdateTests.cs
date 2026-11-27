using System.Reflection;
using System.Text;
using System.Text.Json;
using Cage.Windows;

internal static class CliUpdateTests
{
    // The test controls only stdin. The same updater and wire writer service both the GUI and a cold CLI.
    private sealed class PendingConfirmation : MemoryStream
    {
        public TaskCompletionSource Waiting { get; } = new(TaskCreationOptions.RunContinuationsAsynchronously);
        public TaskCompletionSource CloseInput { get; } = new(TaskCreationOptions.RunContinuationsAsynchronously);
        public override async ValueTask<int> ReadAsync(Memory<byte> buffer, CancellationToken token = default)
        {
            Waiting.TrySetResult();
            await CloseInput.Task.WaitAsync(token);
            return 0;
        }
    }

    private static JsonElement[] Frames(MemoryStream output) => Encoding.UTF8.GetString(output.ToArray())
        .Split('\n', StringSplitOptions.RemoveEmptyEntries).Select(line => JsonSerializer.Deserialize<JsonElement>(line)).ToArray();

    public static async Task Run()
    {
        void Check(bool condition, string message) { if (!condition) throw new Exception(message); }
        using var deadline = new CancellationTokenSource(TimeSpan.FromSeconds(15));
        foreach (var text in new[] { "", "INSTALL", "install\n", "INSTALL\r\n", "INSTALL \n", "OK\n", "INSTALL\n" })
        {
            using var input = new MemoryStream(Encoding.UTF8.GetBytes(text));
            Check(await CliUpdate.ReadInstall(input, deadline.Token) == (text == "INSTALL\n"), "confirmation framing changed");
        }
        var process = UpdateProcess.Identify(Environment.ProcessId, Environment.ProcessPath!);
        using (var pinned = process.Pin(Environment.ProcessPath!)) Check(pinned.Id == Environment.ProcessId, "wrong participant");
        try { using var wrong = (process with { StartTicks = process.StartTicks + 1 }).Pin(Environment.ProcessPath!); throw new Exception("stale PID accepted"); }
        catch (IOException) { }
        try { using var wrong = process.Pin(Path.Combine(Path.GetTempPath(), "different.exe")); throw new Exception("wrong installed executable accepted"); }
        catch (IOException) { }

        using var updater = new AppUpdater(_ => {}, () => throw new Exception("unexpected quit"), () => {}, standaloneCLI: true);
        var plan = new InstallPlan("unused-archive", "unused-digest", "99.0.0", "fixture-publisher", "unused-target", 1, 2, Guid.NewGuid().ToString(), true);
        typeof(AppUpdater).GetField("prepared", BindingFlags.Instance | BindingFlags.NonPublic)!.SetValue(updater, plan);
        using var pending = new PendingConfirmation(); using var owner = new MemoryStream();
        var active = updater.RunCLI(pending, owner, new(null), deadline.Token);
        await pending.Waiting.Task.WaitAsync(deadline.Token);
        Check(updater.Handle("complete").GetValueOrDefault("phase") as string == "ready", "GUI committed the CLI's pending update");
        using var contender = new MemoryStream();
        Check(await updater.RunCLI(Stream.Null, contender, new(null), deadline.Token) == 1 && Frames(contender).Single().Text("phase") == "failed", "parallel CLI acquired update");
        pending.CloseInput.TrySetResult();
        Check(await active == 0, "EOF failed to release the CLI update");
        var states = Frames(owner);
        Check(states.Select(s => s.Text("phase")).SequenceEqual(["ready", "canceled"]), "EOF committed or lost a terminal state");
        Check(states[0].GetProperty("officialUpgrade").GetBoolean(), "official replacement confirmation lost");
        Check(updater.Handle("status").GetValueOrDefault("phase") as string == "ready", "disconnect discarded a GUI-prepared verified update");
        using var progress = new MemoryStream();
        await CliUpdate.Write(progress, new() { ["phase"] = "downloading", ["receivedBytes"] = 3L, ["totalBytes"] = 9L, ["archivePath"] = "private", ["sha256"] = "private" });
        var frame = Frames(progress).Single();
        Check(frame.GetProperty("receivedBytes").GetInt64() == 3 && frame.GetProperty("totalBytes").GetInt64() == 9 &&
            !frame.TryGetProperty("archivePath", out _) && !frame.TryGetProperty("sha256", out _), "progress protocol leaked an internal update field");
        if (OperatingSystem.IsWindows())
        {
            var key = "test-" + Guid.NewGuid().ToString("N");
            using var done = CancellationTokenSource.CreateLinkedTokenSource(deadline.Token);
            var broker = CliUpdatePipe.Serve(key, async (stream, clients, token) => {
                Check(clients.Relay?.Pid == Environment.ProcessId && clients.Terminal == null, "pipe lost the native relay identity");
                await CliUpdate.Write(stream, CliUpdate.Status("ready", "fixture"), token);
                Check(!await CliUpdate.ReadInstall(stream, token), "relay converted EOF into consent");
                await CliUpdate.Write(stream, CliUpdate.Status("canceled", "fixture"), token);
                return 0;
            }, done.Token);
            using var relayOutput = new MemoryStream();
            try
            {
                Check(await CliUpdatePipe.Connect(key, null, Stream.Null, relayOutput) == 0 &&
                    Frames(relayOutput).Select(s => s.Text("phase")).SequenceEqual(["ready", "canceled"]), "desktop pipe relay failed");
            }
            finally { done.Cancel(); await broker; }
        }
        Console.WriteLine("Windows CLI update protocol: confirmation framing, EOF, participant identity, concurrent UI and CLI ownership, and bounded progress fields passed.");
    }
}
