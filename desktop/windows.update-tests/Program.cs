using System.Diagnostics;
using System.Security.Cryptography;
using System.Text.Json;
using Cage.Windows;

// Headless App/backend fixtures use the production updater, helper, signatures
// and NSIS unchanged. Only release transport and workbench readiness are local.
if (args is ["--install-update"])
{
    WriteRecord(Path.Combine(Backend.Root, "helper.json"), new {
        pid = Environment.ProcessId, directory = Environment.CurrentDirectory,
    });
    var exit = await UpdateInstaller.Run(Path.Combine(Backend.Root, "update-installer.json"));
    WriteRecord(Path.Combine(Backend.Root, "helper-finished.json"), new { exit });
    return exit;
}
if (!OperatingSystem.IsWindows()) throw new PlatformNotSupportedException("Run the complete updater transaction on Windows.");

var root = Backend.Root;
Directory.CreateDirectory(root);
using var fixture = JsonDocument.Parse(File.ReadAllText(Path.Combine(root, "fixture.json")));
var settings = fixture.RootElement;
var mode = settings.GetProperty("mode").GetString()!;
void Record(string name, object value) => WriteRecord(Path.Combine(root, name + ".json"), value);

if (args is ["--hold-directory", var directory])
{
    Directory.SetCurrentDirectory(directory);
    // A working directory alone no longer blocks an update: NSIS keeps the
    // installation folder and moves its entries. An open program file
    // without delete sharing still does.
    using var held = new FileStream(Path.Combine(directory, "Cage.install-id"), FileMode.Open, FileAccess.Read, FileShare.Read);
    Record("held", new { pid = Environment.ProcessId });
    while (!File.Exists(Path.Combine(root, "release.json"))) await Task.Delay(100);
    return 0;
}
if (args.Length > 0 && args[0] == "prepare-update")
{
    Record("handoff-" + AppUpdater.Version, new {
        profile = root, version = AppUpdater.Version, executable = Environment.ProcessPath, stop = args.Contains("--stop"),
    });
    Console.WriteLine(JsonSerializer.Serialize(new { status = mode == "cli-active" ? "stop_required" : "prepared" }));
    return 0;
}
if (args is ["app-update", "download"])
{
    var job = Path.Combine(root, "updates", "download-" + Guid.NewGuid());
    Directory.CreateDirectory(job);
    var archive = Path.Combine(job, "Cage-setup.exe");
    File.Copy(settings.GetProperty("installer").GetString()!, archive);
    using var input = File.OpenRead(archive);
    var digest = Convert.ToHexString(SHA256.HashData(input)).ToLowerInvariant();
    if (mode == "digest-failure") digest = new string('0', 64);
    Console.WriteLine(JsonSerializer.Serialize(new {
        phase = "downloaded", currentVersion = AppUpdater.Version, latestVersion = "0.1.8",
        archivePath = archive, sha256 = digest, retryAt = "",
    }));
    return 0;
}
if (args is ["--cli-update"])
    return await CliUpdate.Standalone(Console.OpenStandardInput(), Console.OpenStandardOutput(), CliUpdate.Caller());
if (args is ["--drive-cli"])
{
    // This fixture executes from the installed resources/cage.exe. It holds that
    // image briefly after the real handoff, as a terminal flushing its output can.
    var target = Directory.GetParent(AppContext.BaseDirectory.TrimEnd(Path.DirectorySeparatorChar))!.FullName;
    var start = Backend.StartInfo(Path.Combine(target, "Cage.exe"), "--cli-update");
    start.RedirectStandardInput = true;
    start.Environment["CAGE_UPDATE_CALLER_PID"] = Environment.ProcessId.ToString();
    using var native = Process.Start(start) ?? throw new IOException("CLI update relay failed to start");
    using var deadline = new CancellationTokenSource(TimeSpan.FromMinutes(3));
    bool confirmed = false;
    while (await native.StandardOutput.ReadLineAsync(deadline.Token) is { } line)
    {
        var state = JsonSerializer.Deserialize<JsonElement>(line);
        switch (state.Text("phase"))
        {
            case "ready":
                await native.StandardInput.BaseStream.WriteAsync("INSTALL\n"u8.ToArray(), deadline.Token);
                await native.StandardInput.BaseStream.FlushAsync(deadline.Token);
                confirmed = true; break;
            case "failed":
                native.StandardInput.Close(); await native.WaitForExitAsync(deadline.Token);
                Record("parent-failed", state); return 1;
            case "handed_off":
                await Task.Delay(TimeSpan.FromSeconds(7), deadline.Token);
                var waited = !File.Exists(Path.Combine(root, "update-installer.json"));
                Record("cli-handoff", new { confirmed, waited });
                return confirmed && waited ? 0 : 1;
        }
    }
    return 1;
}
if (args is ["--start-update"])
{
    var failed = new TaskCompletionSource(TaskCreationOptions.RunContinuationsAsynchronously);
    using var updater = new AppUpdater(state => {
        if (state.GetValueOrDefault("phase") as string == "failed")
        {
            Record("parent-failed", state);
            failed.TrySetResult();
        }
    }, () => Environment.Exit(0), () => {});
    updater.Handle("install");
    while (updater.Handle("status").GetValueOrDefault("phase") as string is not ("ready" or "failed")) await Task.Delay(50);
    if (updater.Handle("status").GetValueOrDefault("phase") as string == "ready") updater.Handle("complete");
    await failed.Task.WaitAsync(TimeSpan.FromMinutes(3));
    return 1;
}
if (args.Length != 0) return 2;

if (AppUpdater.Version == "0.1.8")
{
    Record("candidate", new { version = AppUpdater.Version, profile = root, pid = Environment.ProcessId });
    if (mode == "startup-failure")
    {
        File.WriteAllText(Path.Combine(AppContext.BaseDirectory, "my-project", "work.txt"), "saved by new version");
        return 17;
    }
    using var updater = new AppUpdater(_ => {}, () => {}, () => {});
    updater.MarkReady();
    Record("active", new { version = AppUpdater.Version });
}
else Record("reopened", new { version = AppUpdater.Version, profile = root, pid = Environment.ProcessId });

while (!File.Exists(Path.Combine(root, "release.json"))) await Task.Delay(100);
return 0;

static void WriteRecord(string path, object value)
{
    var temporary = path + "." + Guid.NewGuid() + ".tmp";
    File.WriteAllText(temporary, JsonSerializer.Serialize(value));
    File.Move(temporary, path, true);
}
