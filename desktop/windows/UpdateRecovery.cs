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
        var (exit, result) = await Backend.JsonAt(BackendPath(backup), ["prepare-update", "--json", "--stop"], cancellation);
        if (exit != 0 || result.Text("status") != "prepared")
            throw new IOException("旧版后台无法确认环境已停止，原应用备份已保留");
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
