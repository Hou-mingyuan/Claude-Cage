using System.Buffers.Binary;
using System.IO.Pipes;
using System.Runtime.InteropServices;
using System.Text;
using System.Text.Json;
using Microsoft.Win32.SafeHandles;

namespace Cage.Windows;

internal static class CliUpdatePipe
{
    [DllImport("kernel32.dll", SetLastError = true)]
    private static extern bool GetNamedPipeClientProcessId(SafePipeHandle pipe, out int pid);
    [DllImport("kernel32.dll", SetLastError = true)]
    private static extern bool GetNamedPipeServerProcessId(SafePipeHandle pipe, out int pid);

    internal static async Task<int> Start(string key, bool ownsDesktop)
    {
        using var output = Console.OpenStandardOutput();
        using var input = Console.OpenStandardInput();
        try
        {
            var terminal = CliUpdate.Caller();
            return ownsDesktop ? await CliUpdate.Standalone(input, output, terminal) : await Connect(key, terminal, input, output);
        }
        catch (Exception error) { return await CliUpdate.Failure(output, error.Message); }
    }

    internal static async Task<int> Connect(string key, UpdateProcess? terminal, Stream input, Stream output)
    {
        using var pipe = new NamedPipeClientStream(".", "Cage-Update-" + key, PipeDirection.InOut,
            PipeOptions.Asynchronous | PipeOptions.CurrentUserOnly);
        try { await pipe.ConnectAsync(3000); }
        catch (TimeoutException) { return await CliUpdate.Failure(output, "另一个 Cage 正在更新或尚未准备就绪，请稍后重试"); }
        if (!GetNamedPipeServerProcessId(pipe.SafePipeHandle, out int server)) throw new IOException("无法识别工作台更新进程");
        _ = UpdateProcess.Identify(server, Environment.ProcessPath!);
        var header = new byte[12];
        BinaryPrimitives.WriteInt32LittleEndian(header, terminal?.Pid ?? 0);
        BinaryPrimitives.WriteInt64LittleEndian(header.AsSpan(4), terminal?.StartTicks ?? 0);
        await pipe.WriteAsync(header);
        using var reader = new StreamReader(pipe, new UTF8Encoding(false, true), false, leaveOpen: true);
        while (await reader.ReadLineAsync() is { } line)
        {
            if (line.Length > 65536) throw new IOException("工作台更新响应超过允许大小");
            using var record = JsonDocument.Parse(line);
            var phase = record.RootElement.Text("phase");
            await output.WriteAsync(Encoding.UTF8.GetBytes(line + "\n"));
            await output.FlushAsync();
            if (phase == "ready")
            {
                var confirm = await CliUpdate.ReadInstall(input, CancellationToken.None);
                await pipe.WriteAsync(confirm ? "INSTALL\n"u8.ToArray() : "NO\n"u8.ToArray());
            }
            if (phase is "handed_off" or "current" or "manual" or "canceled") return 0;
            if (phase is "failed" or "waiting") return 1;
        }
        throw new IOException("工作台更新连接已关闭");
    }

    internal static async Task Serve(string key, Func<Stream, UpdateClients, CancellationToken, Task<int>> update, CancellationToken lifetime)
    {
        while (!lifetime.IsCancellationRequested)
        {
            using var pipe = new NamedPipeServerStream("Cage-Update-" + key, PipeDirection.InOut, 1,
                PipeTransmissionMode.Byte, PipeOptions.Asynchronous | PipeOptions.CurrentUserOnly);
            try
            {
                await pipe.WaitForConnectionAsync(lifetime);
                if (!GetNamedPipeClientProcessId(pipe.SafePipeHandle, out int peer)) throw new IOException("无法识别终端更新进程");
                var relay = UpdateProcess.Identify(peer, Environment.ProcessPath!);
                var header = new byte[12];
                using (var handshake = CancellationTokenSource.CreateLinkedTokenSource(lifetime))
                {
                    handshake.CancelAfter(TimeSpan.FromSeconds(5));
                    await pipe.ReadExactlyAsync(header, handshake.Token);
                }
                var pid = BinaryPrimitives.ReadInt32LittleEndian(header);
                var ticks = BinaryPrimitives.ReadInt64LittleEndian(header.AsSpan(4));
                if (pid < 0 || ticks < 0 || (pid == 0) != (ticks == 0)) throw new IOException("终端更新调用者无效");
                var terminal = pid == 0 ? null : new UpdateProcess(pid, ticks);
                using var pin = terminal?.Pin(Backend.Executable);
                await update(pipe, new(terminal, relay), lifetime);
            }
            catch (OperationCanceledException) when (lifetime.IsCancellationRequested) { return; }
            catch (Exception error) { if (pipe.IsConnected) await CliUpdate.Failure(pipe, error.Message); }
        }
    }
}
