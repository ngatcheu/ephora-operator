# Installe les outils nécessaires au développement d'ephora-operator sous Windows.
# À lancer depuis un PowerShell administrateur (Chocolatey l'exige) :
#   powershell -ExecutionPolicy Bypass -File hack\install-dev-tools.ps1
#
# Les versions de controller-gen / setup-envtest sont alignées sur le Makefile.

$ErrorActionPreference = "Stop"

$ControllerToolsVersion = "v0.16.5"
$EnvtestVersion         = "release-0.19"
$RepoRoot               = Split-Path -Parent $PSScriptRoot
$LocalBin               = Join-Path $RepoRoot "bin"

$principal = New-Object Security.Principal.WindowsPrincipal([Security.Principal.WindowsIdentity]::GetCurrent())
if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    Write-Error "Lancez ce script depuis un PowerShell administrateur."
}

if (-not (Get-Command choco -ErrorAction SilentlyContinue)) {
    Write-Error "Chocolatey introuvable. Installez-le d'abord : https://chocolatey.org/install"
}

# nom de la commande -> paquet Chocolatey
$tools = [ordered]@{
    "git"       = "git"
    "go"        = "golang"          # go.mod exige Go >= 1.26
    "make"      = "make"
    "kubectl"   = "kubernetes-cli"
    "helm"      = "kubernetes-helm"
    "kustomize" = "kustomize"       # le téléchargement de secours du Makefile ne marche pas sous Git Bash
    "kind"      = "kind"            # cluster local pour `make install` / `make run`
    "docker"    = "docker-desktop"
}

foreach ($cmd in $tools.Keys) {
    if (Get-Command $cmd -ErrorAction SilentlyContinue) {
        Write-Host "[ok]      $cmd déjà installé"
    } else {
        Write-Host "[install] $cmd ($($tools[$cmd]))"
        choco install $tools[$cmd] -y --no-progress
    }
}

# Recharge le PATH modifié par Chocolatey dans la session courante.
$env:Path = [Environment]::GetEnvironmentVariable("Path", "Machine") + ";" + [Environment]::GetEnvironmentVariable("Path", "User")

# Outils Go, installés dans ./bin, là où le Makefile les attend.
New-Item -ItemType Directory -Force $LocalBin | Out-Null
$env:GOBIN = $LocalBin
Write-Host "[install] controller-gen $ControllerToolsVersion"
go install "sigs.k8s.io/controller-tools/cmd/controller-gen@$ControllerToolsVersion"
Write-Host "[install] setup-envtest $EnvtestVersion"
go install "sigs.k8s.io/controller-runtime/tools/setup-envtest@$EnvtestVersion"
Remove-Item Env:GOBIN

Write-Host ""
Write-Host "Versions installées :"
go version
kubectl version --client
helm version --short
kustomize version
kind version
& (Join-Path $LocalBin "controller-gen.exe") --version

Write-Host ""
Write-Host "Terminé. Ouvrez un nouveau terminal (Git Bash recommandé pour make), puis : kind create cluster; make install; make run"
