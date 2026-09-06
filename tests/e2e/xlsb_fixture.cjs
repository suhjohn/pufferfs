// Synthetic workbook written by an independent format writer, never by the
// production Calamine reader. The dependency is pinned in the runner image.
const XLSX = require('/opt/fixture-tools/xlsx.js');
const fs = require('node:fs');
const input = JSON.parse(fs.readFileSync(0, 'utf8'));
const workbook = XLSX.utils.book_new();
for (const [name, cells] of Object.entries(input.sheets)) {
  const sheet = {};
  for (const [address, value] of Object.entries(cells)) {
    sheet[address] = {t: typeof value === 'number' ? 'n' : 's', v: value};
  }
  sheet['!ref'] = input.range;
  XLSX.utils.book_append_sheet(workbook, sheet, name);
}
fs.writeFileSync(input.path, XLSX.write(workbook, {type: 'buffer', bookType: 'xlsb', compression: true}));
