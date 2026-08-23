[CmdletBinding()]
param(
    [string]$Executable
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

if ([string]::IsNullOrWhiteSpace($Executable)) {
    $Executable = Join-Path $PSScriptRoot '.build\windows.exe'
}

if (-not (Test-Path -LiteralPath $Executable -PathType Leaf)) {
    throw "Worker executable not found: $Executable`nRun .\build.bat first."
}

$executablePath = (Resolve-Path -LiteralPath $Executable).Path
$workingDirectory = Split-Path -Parent $executablePath
$workerHost = [Environment]::MachineName
$workers = [System.Collections.Generic.List[System.Diagnostics.Process]]::new()
$launcherExitCode = 0

function Stop-WorkerProcesses {
    # Ctrl+C is delivered to every process attached to this console. Give the
    # Go workers time to release their current jobs and mark heartbeat offline.
    $graceDeadline = [DateTime]::UtcNow.AddSeconds(5)
    do {
        $stillRunning = $false
        foreach ($worker in $workers) {
            try {
                $worker.Refresh()
                if (-not $worker.HasExited) {
                    $stillRunning = $true
                }
            }
            catch {
                # The process already disappeared.
            }
        }
        if ($stillRunning -and [DateTime]::UtcNow -lt $graceDeadline) {
            Start-Sleep -Milliseconds 200
        }
    } while ($stillRunning -and [DateTime]::UtcNow -lt $graceDeadline)

    foreach ($worker in $workers) {
        try {
            $worker.Refresh()
            if (-not $worker.HasExited) {
                Stop-Process -Id $worker.Id -ErrorAction SilentlyContinue
            }
        }
        catch {
            # The worker may have already stopped after receiving Ctrl+C.
        }
    }
}

try {
    foreach ($workerNumber in 1..2) {
        $workerId = "transcode_${workerHost}@${workerNumber}"
        $startInfo = [System.Diagnostics.ProcessStartInfo]::new()
        $startInfo.FileName = $executablePath
        $startInfo.WorkingDirectory = $workingDirectory
        $startInfo.UseShellExecute = $false
        $startInfo.EnvironmentVariables['WORKER_ID'] = $workerId

        $process = [System.Diagnostics.Process]::new()
        $process.StartInfo = $startInfo
        if (-not $process.Start()) {
            throw "Failed to start $workerId"
        }
        $workers.Add($process)
        Write-Host "Started $workerId (PID $($process.Id))" -ForegroundColor Green
    }

    Write-Host ''
    Write-Host 'Two workers are running. Dashboard: http://localhost:8886' -ForegroundColor Cyan
    Write-Host 'Press Ctrl+C to stop both workers.' -ForegroundColor Yellow

    while ($true) {
        foreach ($worker in $workers) {
            $worker.Refresh()
            if ($worker.HasExited) {
                throw "Worker PID $($worker.Id) exited with code $($worker.ExitCode)."
            }
        }
        Start-Sleep -Milliseconds 500
    }
}
catch [System.Management.Automation.PipelineStoppedException] {
    # Normal Ctrl+C shutdown.
}
catch {
    $launcherExitCode = 1
    Write-Error $_
}
finally {
    Write-Host ''
    Write-Host 'Stopping workers...' -ForegroundColor Yellow
    Stop-WorkerProcesses
    foreach ($worker in $workers) {
        $worker.Dispose()
    }
}

exit $launcherExitCode
