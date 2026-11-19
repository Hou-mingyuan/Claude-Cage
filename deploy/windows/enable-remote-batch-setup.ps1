# The dedicated standard account needs this one logon right for offline tool
# installation through Task Scheduler. It does not grant administrator access,
# change credentials, or create/unlock an interactive desktop.
[CmdletBinding()]
param([string]$Profile = 'C:\ProgramData\Cage Remote\windows-guest.json')
$ErrorActionPreference = 'Stop'
$binding = Get-Content -LiteralPath $Profile -Raw -Encoding UTF8 | ConvertFrom-Json
$machine = [guid](Get-ItemPropertyValue -LiteralPath 'HKLM:\SOFTWARE\Microsoft\Cryptography' -Name MachineGuid)
$identity = [Security.Principal.WindowsIdentity]::GetCurrent()
if (-not ([Security.Principal.WindowsPrincipal]::new($identity)).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator) -or $binding.mode -ne 'remote' -or [guid]$binding.machine_id -ne $machine) { throw 'Run on the bound remote machine as its administrator.' }
$account = Get-LocalUser -Name $binding.gui_user
if ($account.SID.Value -ne $binding.gui_user_sid -or $account.SID.Value -eq $identity.User.Value -or (Get-LocalGroupMember -SID 'S-1-5-32-544' | Where-Object { $_.SID.Value -eq $account.SID.Value })) { throw 'Expected the bound standard workbench account.' }
Add-Type -TypeDefinition @'
using System;
using System.ComponentModel;
using System.Runtime.InteropServices;
using System.Security.Principal;
public static class CageBatchLogon {
    [StructLayout(LayoutKind.Sequential)] struct ObjectAttributes { public uint Length; public IntPtr Root,Name; public uint Flags; public IntPtr Security,Quality; }
    [StructLayout(LayoutKind.Sequential)] struct UnicodeString { public ushort Length,Maximum; public IntPtr Buffer; }
    [DllImport("advapi32.dll")] static extern uint LsaOpenPolicy(IntPtr system,ref ObjectAttributes attributes,uint access,out IntPtr policy);
    [DllImport("advapi32.dll")] static extern uint LsaAddAccountRights(IntPtr policy,byte[] sid,ref UnicodeString rights,uint count);
    [DllImport("advapi32.dll")] static extern uint LsaClose(IntPtr policy);
    [DllImport("advapi32.dll")] static extern uint LsaNtStatusToWinError(uint status);
    static void Check(uint status) { if(status!=0)throw new Win32Exception((int)LsaNtStatusToWinError(status)); }
    public static void Enable(string value) {
        var sid=new SecurityIdentifier(value);var bytes=new byte[sid.BinaryLength];sid.GetBinaryForm(bytes,0);
        var attributes=new ObjectAttributes{Length=(uint)Marshal.SizeOf(typeof(ObjectAttributes))};IntPtr policy;
        Check(LsaOpenPolicy(IntPtr.Zero,ref attributes,0x810,out policy));
        const string name="SeBatchLogonRight";
        var right=new UnicodeString{Length=(ushort)(name.Length*2),Maximum=(ushort)((name.Length+1)*2),Buffer=Marshal.StringToHGlobalUni(name)};
        try{Check(LsaAddAccountRights(policy,bytes,ref right,1));}finally{Marshal.FreeHGlobal(right.Buffer);LsaClose(policy);}
    }
}
'@
[CageBatchLogon]::Enable($account.SID.Value)
[pscustomobject]@{user=$account.Name;right='SeBatchLogonRight';purpose='offline_component_setup';interactive_session_started=$false} | ConvertTo-Json -Compress
