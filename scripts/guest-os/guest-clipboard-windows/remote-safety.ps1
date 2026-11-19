# Product safety observations, executed only inside the ordinary-account sandbox.
# Never emit file contents, environment variables or proxy credentials.
$ErrorActionPreference='Stop'
$ProgressPreference='SilentlyContinue'
[Console]::OutputEncoding=[Text.UTF8Encoding]::new($false)
function Native-Code($failure) {
    $errorObject=$failure.Exception
    while ($errorObject.InnerException) { $errorObject=$errorObject.InnerException }
    if ($errorObject -is [Net.Sockets.SocketException]) { return $errorObject.NativeErrorCode }
    if ($errorObject -is [UnauthorizedAccessException]) { return 5 }
    return -1
}
function Socket-Probe([string]$address,[int]$port,[bool]$udp=$false) {
    $ip=[Net.IPAddress]::Parse($address)
    $kind=if($udp){[Net.Sockets.SocketType]::Dgram}else{[Net.Sockets.SocketType]::Stream}
    $protocol=if($udp){[Net.Sockets.ProtocolType]::Udp}else{[Net.Sockets.ProtocolType]::Tcp}
    $socket=[Net.Sockets.Socket]::new($ip.AddressFamily,$kind,$protocol)
    try {
        $null=$socket.ConnectAsync($ip,$port).WaitAsync([TimeSpan]::FromSeconds(2)).GetAwaiter().GetResult()
        if ($udp) {
            $null=$socket.Send([byte[]]@(1,2,3))
            # Datagram rejection can be delivered asynchronously by Winsock.
            # A receive timeout remains inconclusive, never a denial proof.
            $socket.ReceiveTimeout=1500
            $null=$socket.Receive([byte[]]::new(512))
        }
        return 0
    } catch { return (Native-Code $_) }
    finally { $socket.Dispose() }
}
function Fixed-Probe([string]$name) {
    $proxy=[uri]$env:HTTP_PROXY
    $parts=$proxy.UserInfo.Split(':',2)
    $handler=[Net.Http.HttpClientHandler]::new()
    $handler.AllowAutoRedirect=$false
    $handler.Proxy=[Net.WebProxy]::new($proxy.GetLeftPart([UriPartial]::Authority).Replace($proxy.UserInfo+'@',''))
    $handler.Proxy.Credentials=[Net.NetworkCredential]::new($parts[0],$parts[1])
    $client=[Net.Http.HttpClient]::new($handler)
    $client.Timeout=[TimeSpan]::FromSeconds(15)
    try {
        $response=$client.GetAsync('http://198.18.0.254/'+$name).GetAwaiter().GetResult()
        try {
            $body=$response.Content.ReadAsStringAsync().GetAwaiter().GetResult()
            $ip=''
            if($body.Length -le 4096) { try { $ip=($body | ConvertFrom-Json).ip } catch {} }
            return @{status=[int]$response.StatusCode;ip=[string]$ip}
        } finally { $response.Dispose() }
    } catch { return @{status=0;ip=''} }
    finally { $client.Dispose() }
}
try {
    $request=[Console]::ReadLine() | ConvertFrom-Json
    if($request.stage -notin @('environment','boundary','network')) { throw 'invalid_stage' }
    [TimeZoneInfo]::ClearCachedData()
    $zone=[TimeZoneInfo]::Local.Id
    $iana=''; $expected=''
    if([TimeZoneInfo]::TryConvertIanaIdToWindowsId([string]$request.timezone,[ref]$expected) -and $zone -eq $expected) { $zone=[string]$request.timezone }
    elseif([TimeZoneInfo]::TryConvertWindowsIdToIanaId($zone,[ref]$iana)) { $zone=$iana }
    # The registry can already contain a locale pending a reboot. Read the
    # active Windows API value, not that staged value, as system evidence.
    Add-Type -TypeDefinition @'
using System.Runtime.InteropServices;
using System.Text;
public static class CageActiveLocale {
    [DllImport("kernel32.dll", CharSet=CharSet.Unicode)]
    public static extern int GetSystemDefaultLocaleName(StringBuilder value, int count);
    [DllImport("kernel32.dll")] public static extern uint GetACP();
}
'@
    $localeBuffer=[Text.StringBuilder]::new(85)
    $localeLength=[CageActiveLocale]::GetSystemDefaultLocaleName($localeBuffer,85)
    $systemLocale=if($localeLength -gt 0){$localeBuffer.ToString()}else{''}
    $evidence=@{}
    $evidence.system_code_page=[CageActiveLocale]::GetACP()
    $result=@{tz=$zone;system_timezone=$zone;process_locale=[Globalization.CultureInfo]::CurrentCulture.Name;system_locale=$systemLocale;locale_valid=($systemLocale.Length -gt 0);remote=$evidence}
    if($request.stage -eq 'network') {
        $result.egress=Fixed-Probe 'egress'
        $result.dns=Fixed-Probe 'dns'
    }
    if($request.stage -eq 'boundary') {
        $port=[int]$request.control_port
        $proxyPort=if ($null -eq $request.proxy_port) { 17896 } else { [int]$request.proxy_port }
        if ($proxyPort -lt 1 -or $proxyPort -gt 65535) { throw 'invalid_proxy_port' }
        $evidence.proxy_port=$proxyPort
        $file=[string]$request.private_file
        $nonce=[string]$request.nonce
        if($port -lt 1 -or $port -gt 65535 -or $nonce -notmatch '^[a-f0-9]{64}$' -or
            [IO.Path]::GetFileName($file) -notmatch '^\.cage-private-probe-' -or -not [IO.Path]::IsPathFullyQualified($file)) { throw 'invalid_boundary' }
        $evidence.tcp=@{
            public=(Socket-Probe '1.1.1.1' 443)
            management=(Socket-Probe '127.0.0.1' $port)
            upstream=(Socket-Probe '127.0.0.1' $proxyPort)
            metadata=(Socket-Probe '169.254.169.254' 80)
            dns=(Socket-Probe '1.1.1.1' 53)
            dot=(Socket-Probe '1.1.1.1' 853)
            ipv6=(Socket-Probe '2606:4700:4700::1111' 443)
            loopback6=(Socket-Probe '::1' $port)
            metadata6=(Socket-Probe 'fd00:ec2::254' 80)
        }
        $evidence.udp4=Socket-Probe '1.1.1.1' 53 $true
        $evidence.udp6=Socket-Probe '2606:4700:4700::1111' 53 $true
        try { $null=[Net.Dns]::GetHostAddressesAsync('www.cloudflare.com').WaitAsync([TimeSpan]::FromSeconds(3)).GetAwaiter().GetResult();$evidence.system_dns=0 }
        catch { $evidence.system_dns=Native-Code $_ }
        $stream=$null
        try { $stream=[IO.File]::OpenRead($file);$evidence.private_read=0 }
        catch { $evidence.private_read=Native-Code $_ }
        finally { if($stream){$stream.Dispose()} }
        $owned=Join-Path $env:USERPROFILE ('.cage-home-probe-'+$nonce)
        try { [IO.File]::WriteAllText($owned,$nonce);$evidence.home_roundtrip=([IO.File]::ReadAllText($owned) -ceq $nonce) }
        finally { if([IO.File]::Exists($owned)){[IO.File]::Delete($owned)} }
        $evidence.nonce=$nonce
    }
    [Console]::WriteLine(($result | ConvertTo-Json -Depth 8 -Compress))
} catch {
    [Console]::WriteLine('{"error":"remote_safety_probe_failed"}')
    exit 1
}
