from pathlib import Path
import json
import subprocess

here = Path(__file__).resolve().parent
root = here.parents[3]
out = root / '.hive-tmp/library-audit'
consumer = out / 'consumer'
consumer.mkdir(parents=True, exist_ok=True)
logs = []

def run(cmd, cwd, timeout):
    result = subprocess.run(cmd, cwd=cwd, capture_output=True, text=True, timeout=timeout)
    logs.append('$ ' + ' '.join(cmd) + '\n' + result.stdout + result.stderr + '\nexit=' + str(result.returncode))
    if result.returncode:
        (here / 'consumer-output.txt').write_text('\n'.join(logs))
        raise SystemExit(result.returncode)
    return result.stdout

pack = run(['npm', 'pack', '--workspace', '@azex/ledger-react', '--pack-destination', str(out), '--json'], root / 'web', 120)
tarball = out / json.loads(pack)[0]['filename']
(consumer / 'package.json').write_text(json.dumps({
    'name': 'ledger-external-audit', 'private': True, 'type': 'module',
    'dependencies': {'@azex/ledger-react': str(tarball), '@tanstack/react-query': '^5', 'react': '^19', 'react-dom': '^19'},
    'devDependencies': {'typescript': '5.9.3', '@types/react': '^19', '@types/react-dom': '^19'},
}, indent=2))
(consumer / 'index.ts').write_text('''import { LedgerProvider, createLedgerClient } from "@azex/ledger-react";
import { createServerLedgerClient } from "@azex/ledger-react/server";
import { WalletProvider, WalletPanel } from "@azex/ledger-react/wallet";
import { useBalances } from "@azex/ledger-react/headless";
void [LedgerProvider, createLedgerClient, createServerLedgerClient, WalletProvider, useBalances, WalletPanel];
''')
(consumer / 'tsconfig.json').write_text(json.dumps({'compilerOptions': {'target': 'es2022', 'module': 'esnext', 'moduleResolution': 'bundler', 'strict': True, 'skipLibCheck': True, 'noEmit': True}, 'include': ['index.ts']}))
run(['npm', 'install', '--ignore-scripts', '--omit=optional', '--no-audit', '--no-fund'], consumer, 600)
run([str(consumer / 'node_modules/.bin/tsc'), '--noEmit'], consumer, 120)
run(['node', '--input-type=module', '-e', 'for (const name of ["@azex/ledger-react", "@azex/ledger-react/server", "@azex/ledger-react/headless", "@azex/ledger-react/charts", "@azex/ledger-react/wallet", "@azex/ledger-react/wallet/headless"]) { await import(name); console.log("loaded", name); }'], consumer, 120)
hero = (consumer / 'node_modules/@heroui/react').exists()
assert not hero, 'HeroUI must not be installed for this check'
logs.append('External tarball consumer succeeded; HeroUI installed = False')
text = '\n'.join(logs) + '\n'
(here / 'consumer-output.txt').write_text(text)
print('\n'.join(logs[1:]))
