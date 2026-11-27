using System.Diagnostics;
using System.Text.Json;
using System.Threading.Channels;

namespace Cage.Windows;

internal sealed record UpdateProcess(int Pid, long StartTicks)
{
    public static UpdateProcess Identify(int pid, string expectedPath)
    {
        using var process = Open(pid, expectedPath);
        return new(pid, process.StartTime.ToUniversalTime().Ticks);
    }

    public Process Pin(string expectedPath)
    {
        var process = Open(Pid, expectedPath);
        try
        {
            if (process.StartTime.ToUniversalTime().Ticks != StartTicks) throw new IOException("更新请求进程已经改变");
            return process;
        }
        catch { process.Dispose(); throw; }
    }

    private static Process Open(int pid, string expectedPath)
    {
        var process = Process.GetProcessById(pid);
        try
        {
            // Keep the kernel handle, including its creation time, through handoff.
            var image = Backend.ProcessImage(process);
            if (process.HasExited || !string.Equals(image, expectedPath, StringComparison.OrdinalIgnoreCase))
                throw new IOException("更新请求进程身份不匹配");
            return process;
        }
        catch { process.Dispose(); throw; }
    }
}

internal sealed record UpdateClients(UpdateProcess? Terminal, UpdateProcess? Relay = null);

internal static class CliUpdate
{
    internal static UpdateProcess? Caller()
    {
        var value = Environment.GetEnvironmentVariable("CAGE_UPDATE_CALLER_PID");
        if (value == null) return null;
        Environment.SetEnvironmentVariable("CAGE_UPDATE_CALLER_PID", null);
        if (!int.TryParse(value, out int pid) || pid <= 0) throw new IOException("终端更新调用者无效");
        return UpdateProcess.Identify(pid, Backend.Executable);
    }

    internal static async Task<int> Standalone(Stream input, Stream output, UpdateProcess? caller)
    {
        using var updater = new AppUpdater(_ => {}, () => {}, () => {}, standaloneCLI: true);
        return await updater.RunCLI(input, output, new(caller));
    }

    internal static async Task<bool> ReadInstall(Stream input, CancellationToken token)
    {
        var command = "INSTALL\n"u8.ToArray();
        var bytes = new byte[command.Length];
        var offset = 0;
        while (offset < bytes.Length)
        {
            int count = await input.ReadAsync(bytes.AsMemory(offset), token);
            if (count == 0 || !bytes.AsSpan(offset, count).SequenceEqual(command.AsSpan(offset, count))) return false;
            offset += count;
        }
        return true;
    }

    private static readonly string[] WireFields = ["phase", "message", "currentVersion", "latestVersion", "receivedBytes", "totalBytes", "officialUpgrade", "retryAt"];

    internal static async Task Write(Stream output, Dictionary<string, object?> state, CancellationToken token = default)
    {
        // The desktop state may also contain paths and provider metadata. The CLI
        // protocol carries only the release identity, status and transfer progress.
        var wire = new Dictionary<string, object?>();
        foreach (var key in WireFields)
            if (state.TryGetValue(key, out var value)) wire[key] = value;
        await output.WriteAsync(JsonSerializer.SerializeToUtf8Bytes(wire), token).ConfigureAwait(false);
        await output.WriteAsync("\n"u8.ToArray(), token).ConfigureAwait(false);
        await output.FlushAsync(token).ConfigureAwait(false);
    }

    internal static Dictionary<string, object?> Status(string phase, string message) =>
        new() { ["phase"] = phase, ["message"] = message, ["currentVersion"] = AppUpdater.Version };

    internal static async Task<int> Failure(Stream output, string message)
    {
        try { await Write(output, Status("failed", message)); } catch (Exception error) when (error is IOException or ObjectDisposedException) { }
        return 1;
    }

    internal static async Task WriteProgress(ChannelReader<Dictionary<string, object?>> states, Stream output, CancellationToken token)
    {
        await foreach (var s in states.ReadAllAsync(token).ConfigureAwait(false))
            await Write(output, s, token).ConfigureAwait(false);
    }
}
