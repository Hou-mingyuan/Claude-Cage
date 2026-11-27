using System.Collections.Concurrent;
using System.Reflection;
using System.Security.Cryptography;
using System.Text.Json;
using Cage.Windows;

internal static class AppUpdaterTests
{
    public static bool BackendFixture(string operation)
    {
        var phase = Environment.GetEnvironmentVariable("CAGE_UPDATER_TEST_PHASE");
        if (phase == null) return false;
        File.AppendAllText(Path.Combine(Backend.Root, "update-requests"), operation + "\n");
        if (phase == "early-current") { Console.WriteLine("""{"phase":"current"}"""); Environment.Exit(1); }
        if (phase == "invalid-handoff")
        {
            // Keep the file open after the invalid result until the updater stops this process.
            using var held = File.Open(Path.Combine(Backend.Root, "downloader.lock"), FileMode.Create, FileAccess.Write, FileShare.None);
            Console.WriteLine("""{"phase":"handed_off"}""");
            Thread.Sleep(TimeSpan.FromSeconds(10));
            return true;
        }
        Console.WriteLine(JsonSerializer.Serialize(new {
            phase = operation == "download" && phase != "current" ? "failed" : phase,
            latestVersion = "99.0.0", message = "fixture response",
            retryAt = phase == "failed" ? DateTimeOffset.UtcNow.AddHours(1).ToString("O") : "",
        }));
        if (phase == "failed") Environment.Exit(1);
        return true;
    }

    public static async Task Run()
    {
        var root = Path.Combine(Path.GetTempPath(), "cage-updater-test-" + Guid.NewGuid());
        var resources = Path.Combine(AppContext.BaseDirectory, "resources");
        if (Directory.Exists(resources)) throw new IOException("updater fixture requires an unused resources directory");
        var savedProfile = Environment.GetEnvironmentVariable("CLAUDE_CAGE_CONFIG");
        var savedPhase = Environment.GetEnvironmentVariable("CAGE_UPDATER_TEST_PHASE");
        Directory.CreateDirectory(root);
        Directory.CreateDirectory(resources);
        try
        {
            Environment.SetEnvironmentVariable("CLAUDE_CAGE_CONFIG", Path.Combine(root, "config.json"));
            foreach (var file in Directory.EnumerateFiles(AppContext.BaseDirectory))
                File.Copy(file, Path.Combine(resources, Path.GetFileName(file)));
            File.Copy(Environment.ProcessPath!, Backend.Executable, true);
            if (!OperatingSystem.IsWindows()) File.SetUnixFileMode(Backend.Executable, File.GetUnixFileMode(Environment.ProcessPath!));
            var requests = Path.Combine(root, "update-requests");
            var phases = new ConcurrentQueue<string>();
            void Check(bool value, string message) { if (!value) throw new Exception(message); }
            async Task<Dictionary<string, object?>> Exercise(string operation, string backendPhase, Func<string>? publisher, string expectedOperation, Func<bool>? canUpgradeUnsigned = null)
            {
                phases.Clear();
                File.WriteAllText(requests, "");
                Environment.SetEnvironmentVariable("CAGE_UPDATER_TEST_PHASE", backendPhase);
                var downloaderReleased = false;
                void OnChange(Dictionary<string, object?> state)
                {
                    var phase = state.GetValueOrDefault("phase") as string ?? "";
                    phases.Enqueue(phase);
                    if (backendPhase == "invalid-handoff" && phase == "failed")
                    {
                        try
                        {
                            using var probe = File.OpenRead(Path.Combine(root, "downloader.lock"));
                            downloaderReleased = true;
                        }
                        catch (IOException) { }
                    }
                }
                using var updater = new AppUpdater(OnChange, () => throw new Exception("unexpected installation"),
                    () => throw new Exception("unexpected restart"), publisher, null, canUpgradeUnsigned);
                updater.Handle(operation);
                // Wait for both terminal state publication and subprocess cleanup.
                var running = typeof(AppUpdater).GetField("cancellation", BindingFlags.Instance | BindingFlags.NonPublic)!;
                using var deadline = new CancellationTokenSource(TimeSpan.FromSeconds(15));
                while (running.GetValue(updater) != null) await Task.Delay(10, deadline.Token);
                var state = updater.Handle("status");
                if (backendPhase == "invalid-handoff")
                    Check(downloaderReleased, "update failed before the downloader released its files");
                Check(File.ReadAllText(requests) == expectedOperation + "\n", "unexpected backend operation");
                if (backendPhase == "failed")
                {
                    Check(!string.IsNullOrEmpty(state.GetValueOrDefault("retryAt") as string), "retry deadline lost");
                    updater.Handle("check"); updater.Handle("install");
                    await Task.Delay(100);
                    Check(File.ReadAllText(requests) == expectedOperation + "\n", "cooldown bypassed");
                }
                return state;
            }

            // On Windows exercise the real WinVerifyTrust path with this unsigned executable.
            // Without Cage.install-id in AppContext.BaseDirectory, default unsigned upgrade is disabled.
            Func<string>? unsigned = OperatingSystem.IsWindows() ? null : () => throw new IOException("unsigned fixture");
            foreach (var operation in new[] { "check", "install" })
            {
                var state = await Exercise(operation, "available", unsigned, "check");
                Check(state["phase"] as string == "manual", "unsigned build did not offer manual update");
                Check(state["latestVersion"] as string == "99.0.0", "manual update lost release version");
                Check(state.GetValueOrDefault("officialUpgrade") as bool? != true, "manual update must not advertise officialUpgrade");
                Check(!phases.Contains("available") && !phases.Contains("downloading"), "unsigned build exposed automatic installation");
                Check((state["message"] as string)?.Contains("官方发布页") == true, "manual update omitted release guidance");
            }
            var untrusted = await Exercise("install", "available", () => throw new CryptographicException("untrusted"), "check");
            Check(untrusted["phase"] as string == "manual", "unverifiable signer allowed a download");
            var current = await Exercise("check", "current", unsigned, "check");
            Check(current["phase"] as string == "current", "current build showed a spurious manual update");
            var unsignedUpgradeCheck = await Exercise("check", "available", unsigned, "check", () => true);
            Check(unsignedUpgradeCheck["phase"] as string == "available", "eligible unsigned build did not offer official upgrade");
            Check(unsignedUpgradeCheck.GetValueOrDefault("officialUpgrade") as bool? == true, "eligible unsigned build did not mark officialUpgrade");
            await Exercise("install", "available", unsigned, "download", () => true);
            var unsignedUpgradeCurrent = await Exercise("check", "current", unsigned, "check", () => true);
            Check(unsignedUpgradeCurrent.GetValueOrDefault("officialUpgrade") as bool? != true, "current phase must clear officialUpgrade");
            var signed = await Exercise("check", "available", () => "fixture-publisher", "check", () => true);
            Check(signed["phase"] as string == "available", "trusted publisher lost automatic update option");
            Check(signed.GetValueOrDefault("officialUpgrade") as bool? != true, "signed build must not mark officialUpgrade");
            await Exercise("install", "available", () => "fixture-publisher", "download", () => true);
            var failed = await Exercise("check", "failed", unsigned, "check");
            Check(failed["phase"] as string == "failed", "network failure was hidden as manual update");
            async Task<string> TerminalPhase(string backendPhase, Func<string> certificate, int expectedExit)
            {
                File.WriteAllText(requests, "");
                Environment.SetEnvironmentVariable("CAGE_UPDATER_TEST_PHASE", backendPhase);
                using var cli = new AppUpdater(_ => {}, () => throw new Exception("unexpected installation"), () => {}, certificate, canUpgradeUnsigned: () => false);
                using var output = new MemoryStream();
                if (await cli.RunCLI(Stream.Null, output, new(null)) != expectedExit) throw new Exception("unexpected CLI update exit");
                var lines = System.Text.Encoding.UTF8.GetString(output.ToArray()).Split('\n', StringSplitOptions.RemoveEmptyEntries);
                return JsonSerializer.Deserialize<JsonElement>(lines[^1]).Text("phase");
            }
            Check(await TerminalPhase("available", () => throw new CryptographicException("untrusted"), 0) == "manual" &&
                File.ReadAllText(requests) == "check\n", "manual CLI path downloaded an untrusted package");
            Check(await TerminalPhase("current", () => "fixture-publisher", 0) == "current" &&
                File.ReadAllText(requests) == "download\n", "cold CLI did not finish when already current");
            Check(await TerminalPhase("failed", () => "fixture-publisher", 1) == "failed", "CLI lost a backend failure");
            foreach (var (fixture, outcome) in new[] { ("early-current", "current"), ("invalid-handoff", "handed_off") })
            {
                var state = await Exercise("install", fixture, () => "fixture-publisher", "download");
                Check((string?)state["phase"] == "failed" && !phases.Contains(outcome),
                    "an incomplete download or a downloader event was allowed to declare the update result");
            }
            Console.WriteLine("Windows updater eligibility: unsigned/untrusted manual fallback, eligible official upgrade, trusted download and cooldown passed.");
        }
        finally
        {
            Environment.SetEnvironmentVariable("CLAUDE_CAGE_CONFIG", savedProfile);
            Environment.SetEnvironmentVariable("CAGE_UPDATER_TEST_PHASE", savedPhase);
            Directory.Delete(resources, true);
            Directory.Delete(root, true);
        }
    }
}
