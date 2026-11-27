using System.ComponentModel;
using System.Diagnostics;
using System.Security.Cryptography;

namespace Cage.Windows;

internal static class UpdateRecovery
{
    public static string BackendPath(string backup) => Path.Combine(backup, "resources", "cage.exe");

    // NSIS may reject an update or restore its original directory before it
    // exits. A started installer alone does not mean the App was replaced.
    public static bool OriginalIntact(string target, string backup)
    {
        try
        {
            if (File.Exists(target + ".cage-installation.json") || !Directory.Exists(target) || !Directory.Exists(backup)) return false;
            UpdatePolicy.PlainPath(target); UpdatePolicy.PlainPath(backup);
            if (!File.Exists(Path.Combine(backup, "Cage.exe")) || !File.Exists(Path.Combine(backup, "Cage.install-id"))) return false;
            foreach (var source in Directory.EnumerateFiles(backup, "*", SearchOption.AllDirectories))
            {
                var installed = Path.Combine(target, Path.GetRelativePath(backup, source));
                UpdatePolicy.PlainPath(source); UpdatePolicy.PlainPath(installed);
                using var before = File.OpenRead(source);
                using var after = File.OpenRead(installed);
                if (before.Length != after.Length || !SHA256.HashData(before).AsSpan().SequenceEqual(SHA256.HashData(after))) return false;
            }
            return true;
        }
        catch (IOException) { return false; }
        catch (UnauthorizedAccessException) { return false; }
    }

    // The installed candidate may be unable to start. Use the existing backup's
    // verified controller to check the shared profile and stop any new process.
    public static async Task PrepareRollback(string backup, CancellationToken cancellation = default)
    {
        var (exit, result) = await Backend.JsonAt(BackendPath(backup), ["prepare-update", "--json", "--stop"], cancellation, Backend.UpdateStopDeadline);
        if (exit != 0 || result.Text("status") != "prepared")
            throw new IOException("旧版后台无法确认环境已停止，原应用备份已保留");
    }

    // A second start hands its window to the App already running and exits, so
    // the launched process need not be the one showing the candidate. An App
    // left running from the installation restarts the controller that rollback
    // stopped and holds program files open. Only its exact launcher path is
    // matched; other Cage installations keep running.
    public static async Task<bool> StopInstalledApps(string target)
    {
        var launcher = Path.Combine(target, "Cage.exe");
        var stopped = false;
        foreach (var process in Process.GetProcessesByName(Path.GetFileNameWithoutExtension(launcher)))
            using (process)
            {
                if (!string.Equals(ImagePath(process), launcher, StringComparison.OrdinalIgnoreCase)) continue;
                try
                {
                    process.Kill(); stopped = true;
                    await process.WaitForExitAsync().WaitAsync(TimeSpan.FromSeconds(10));
                }
                catch (Exception error) when (error is Win32Exception or InvalidOperationException or TimeoutException) { }
            }
        return stopped;
    }

    // Rollback moves the installed program folders. Windows refuses to move a
    // folder while a process runs an image inside it, which would leave the
    // candidate half detached. Wait for the stopped controller's children.
    public static async Task WaitForInstalledProcesses(string target, TimeSpan timeout)
    {
        var prefix = Path.GetFullPath(target).TrimEnd(Path.DirectorySeparatorChar) + Path.DirectorySeparatorChar;
        var deadline = DateTime.UtcNow + timeout;
        while (RunningFrom(prefix) is { } running)
        {
            if (DateTime.UtcNow >= deadline)
                throw new IOException("新版 Cage 的 " + running + " 仍在运行，安装目录尚未改动。请退出 Cage 后重试恢复");
            await Task.Delay(250);
        }
    }

    // Detach moves program folders even while a process runs an image inside
    // them, so a candidate started after the installation check runs from the
    // failed folder, where cleanup can neither delete nor move its files. Stop
    // it as before detach. One that keeps running leaves its unchanged program
    // files to the cleanup retention path rather than failing the rollback.
    public static async Task StopDetachedCandidate(string backup, string failed, TimeSpan timeout)
    {
        var prefix = Path.GetFullPath(failed).TrimEnd(Path.DirectorySeparatorChar) + Path.DirectorySeparatorChar;
        if (!await StopInstalledApps(failed) && RunningFrom(prefix) == null) return;
        try { await PrepareRollback(backup); }
        catch (Exception error) when (error is IOException or OperationCanceledException or Win32Exception or UnauthorizedAccessException) { }
        try { await WaitForInstalledProcesses(failed, timeout); }
        catch (IOException) { }
    }

    // NSIS refuses to start while an earlier transaction awaits recovery and
    // only reports exit code 2, which hides why the update cannot proceed.
    public static void EnsureNoPendingTransaction(string target)
    {
        var receipt = target + ".cage-installation.json";
        if (File.Exists(receipt))
            throw new IOException("上一次安装或更新尚未完成恢复，本次更新未开始，原应用未改动。请先处理 " + receipt + " 中记录的目录");
    }

    private static string? RunningFrom(string prefix)
    {
        foreach (var process in Process.GetProcesses())
            using (process)
                if (ImagePath(process) is { } path && path.StartsWith(prefix, StringComparison.OrdinalIgnoreCase))
                    return Path.GetFileName(path);
        return null;
    }

    private static string? ImagePath(Process process)
    {
        try { return process.MainModule?.FileName; }
        catch (Exception error) when (error is Win32Exception or InvalidOperationException or NotSupportedException) { return null; }
    }

    public static void CleanupDirectory(string? path)
    {
        if (path == null) return;
        try { Directory.Delete(path, true); }
        catch (IOException) { }
        catch (UnauthorizedAccessException) { }
    }

    // This only runs after workbench acknowledgement. Cleanup failures leave
    // files for later removal and cannot change the installation result.
    public static void CleanupAfterSuccess(string backup, string download, string marker)
    {
        CleanupDirectory(backup);
        CleanupDirectory(download);
        try { File.Delete(marker); }
        catch (IOException) { }
        catch (UnauthorizedAccessException) { }
    }
}
