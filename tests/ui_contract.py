#!/usr/bin/env python3
"""Small dependency-free UI wiring smoke check for CI."""
from html.parser import HTMLParser
from pathlib import Path
import re

UI = Path('internal/server/ui')
class Collector(HTMLParser):
    def __init__(self):
        super().__init__()
        self.ids = set()
        self.scripts = []
    def handle_starttag(self, tag, attrs):
        attrs = dict(attrs)
        if 'id' in attrs:
            assert attrs['id'] not in self.ids, f'duplicate HTML ID: {attrs["id"]}'
            self.ids.add(attrs['id'])
        if tag == 'script':
            self.scripts.append(attrs.get('src'))

html = Collector()
html.feed((UI/'index.html').read_text())
assert '/ui/subscriptions.js' in html.scripts
assert '/ui/library.js' in html.scripts
assert '/ui/app.js' in html.scripts
assert '/ui/admin.js' in html.scripts
for script in ('app.js', 'library.js', 'subscriptions.js', 'admin.js'):
    code = (UI/script).read_text()
    needed = set(re.findall(r"\$\(['\"]([^'\"]+)['\"]\)", code))
    missing = sorted(needed - html.ids)
    assert not missing, f'{script} references missing HTML elements: {missing}'
admin = (UI/'admin.js').read_text()
assert 'renderAdminHardware(data.hardware)' in admin, 'admin hardware summary not connected'
assert 'adminVideoEnabled' in admin and 'adminSavePolicy' in admin, 'explicit admin confirmation missing'
assert {'adminHardwareReport', 'adminVideoProbe', 'adminVideoBackend'}.issubset(html.ids)
app = (UI/'app.js').read_text()
assert "block.type==='unsupported'" in app, 'unknown Boosty blocks are silently discarded from live post UI'
assert 'Неподдерживаемый блок Boosty' in app, 'unknown post block explanation missing'
print('UI wiring: pass')
