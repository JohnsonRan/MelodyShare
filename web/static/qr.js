/*
 * Minimal QR Code Model 2 encoder for byte-mode URLs, error correction level L.
 * Based on the QR Code specification; this implementation is released under MIT-0.
 */
'use strict';

window.QRCode = (() => {
  const RS_BLOCKS = [
    [[1, 26, 19]], [[1, 44, 34]], [[1, 70, 55]], [[1, 100, 80]],
    [[1, 134, 108]], [[2, 86, 68]], [[2, 98, 78]], [[2, 121, 97]],
    [[2, 146, 116]], [[2, 86, 68], [2, 87, 69]],
  ];
  const ALIGNMENT = [[], [6, 18], [6, 22], [6, 26], [6, 30], [6, 34], [6, 22, 38], [6, 24, 42], [6, 26, 46], [6, 28, 50]];
  const EXP = new Uint8Array(512);
  const LOG = new Uint8Array(256);
  let value = 1;
  for (let i = 0; i < 255; i++) {
    EXP[i] = value;
    LOG[value] = i;
    value = (value << 1) ^ (value & 0x80 ? 0x11d : 0);
  }
  for (let i = 255; i < 512; i++) EXP[i] = EXP[i - 255];

  const multiply = (a, b) => (a && b ? EXP[LOG[a] + LOG[b]] : 0);
  const bch = (data, polynomial) => {
    let value = data;
    const degree = Math.floor(Math.log2(polynomial));
    while (Math.floor(Math.log2(value)) >= degree) {
      value ^= polynomial << (Math.floor(Math.log2(value)) - degree);
    }
    return value;
  };
  const utf8Bytes = (text) => new TextEncoder().encode(text);

  function dataCodewords(bytes, version) {
    const capacity = RS_BLOCKS[version - 1].reduce((sum, block) => sum + block[0] * block[2], 0);
    const bitLength = 4 + (version < 10 ? 8 : 16) + bytes.length * 8;
    if (bitLength > capacity * 8) return null;
    const bits = [];
    const push = (number, length) => {
      for (let i = length - 1; i >= 0; i--) bits.push((number >>> i) & 1);
    };
    push(4, 4); // byte mode
    push(bytes.length, version < 10 ? 8 : 16);
    for (const byte of bytes) push(byte, 8);
    for (let i = 0; i < Math.min(4, capacity * 8 - bits.length); i++) bits.push(0);
    while (bits.length % 8) bits.push(0);
    const words = [];
    for (let i = 0; i < bits.length; i += 8) {
      let word = 0;
      for (let j = 0; j < 8; j++) word = (word << 1) | bits[i + j];
      words.push(word);
    }
    for (let pad = 0; words.length < capacity; pad++) words.push(pad % 2 ? 0x11 : 0xec);
    return words;
  }

  function remainder(words, eccLength) {
    const generator = [1];
    for (let i = 0; i < eccLength; i++) {
      generator.push(0);
      for (let j = generator.length - 1; j > 0; j--) generator[j] = generator[j - 1] ^ multiply(generator[j], EXP[i]);
      generator[0] = multiply(generator[0], EXP[i]);
    }
    const result = new Uint8Array(eccLength);
    for (const word of words) {
      const factor = word ^ result[0];
      result.copyWithin(0, 1);
      result[eccLength - 1] = 0;
      for (let i = 0; i < eccLength; i++) result[i] ^= multiply(generator[i + 1], factor);
    }
    return [...result];
  }

  function interleave(words, version) {
    const blocks = [];
    let offset = 0;
    for (const [count, total, data] of RS_BLOCKS[version - 1]) {
      for (let i = 0; i < count; i++) {
        const part = words.slice(offset, offset + data);
        offset += data;
        blocks.push([part, remainder(part, total - data)]);
      }
    }
    const result = [];
    for (let i = 0; i < Math.max(...blocks.map(([data]) => data.length)); i++) for (const [data] of blocks) if (i < data.length) result.push(data[i]);
    for (let i = 0; i < Math.max(...blocks.map(([, ecc]) => ecc.length)); i++) for (const [, ecc] of blocks) if (i < ecc.length) result.push(ecc[i]);
    return result;
  }

  function makeMatrix(version, words, mask) {
    const size = version * 4 + 17;
    const modules = Array.from({ length: size }, () => Array(size).fill(null));
    const set = (row, col, value) => { if (row >= 0 && row < size && col >= 0 && col < size) modules[row][col] = value; };
    const finder = (row, col) => {
      for (let r = -1; r <= 7; r++) for (let c = -1; c <= 7; c++) set(row + r, col + c, r >= 0 && r <= 6 && c >= 0 && c <= 6 && (r === 0 || r === 6 || c === 0 || c === 6 || (r >= 2 && r <= 4 && c >= 2 && c <= 4)));
    };
    finder(0, 0); finder(size - 7, 0); finder(0, size - 7);
    for (let i = 8; i < size - 8; i++) { set(6, i, i % 2 === 0); set(i, 6, i % 2 === 0); }
    for (const row of ALIGNMENT[version - 1]) for (const col of ALIGNMENT[version - 1]) {
      if (modules[row][col] !== null) continue;
      for (let r = -2; r <= 2; r++) for (let c = -2; c <= 2; c++) set(row + r, col + c, Math.abs(r) === 2 || Math.abs(c) === 2 || (r === 0 && c === 0));
    }
    const format = ((1 << 3) | mask) << 10 | bch(((1 << 3) | mask) << 10, 0x537) ^ 0x5412;
    for (let i = 0; i < 15; i++) {
      const bit = ((format >>> i) & 1) === 1;
      set(i < 6 ? i : i < 8 ? i + 1 : size - 15 + i, 8, bit);
      set(8, i < 8 ? size - i - 1 : i < 9 ? 15 - i : 14 - i, bit);
    }
    set(size - 8, 8, true);
    if (version >= 7) {
      const number = (version << 12) | bch(version << 12, 0x1f25);
      for (let i = 0; i < 18; i++) {
        const bit = ((number >>> i) & 1) === 1;
        set(Math.floor(i / 3), size - 11 + (i % 3), bit);
        set(size - 11 + (i % 3), Math.floor(i / 3), bit);
      }
    }
    let bit = 0;
    let upward = true;
    const masked = (row, col) => [((row + col) % 2) === 0, row % 2 === 0, col % 3 === 0, (row + col) % 3 === 0, (Math.floor(row / 2) + Math.floor(col / 3)) % 2 === 0, (row * col) % 2 + (row * col) % 3 === 0, ((row * col) % 2 + (row * col) % 3) % 2 === 0, ((row * col) % 3 + (row + col) % 2) % 2 === 0][mask];
    for (let col = size - 1; col > 0; col -= 2) {
      if (col === 6) col--;
      for (let index = 0; index < size; index++) {
        const row = upward ? size - 1 - index : index;
        for (let c = 0; c < 2; c++) if (modules[row][col - c] === null) {
          const raw = bit < words.length * 8 && ((words[Math.floor(bit / 8)] >>> (7 - bit % 8)) & 1) === 1;
          modules[row][col - c] = raw !== masked(row, col - c);
          bit++;
        }
      }
      upward = !upward;
    }
    return modules;
  }

  function penalty(matrix) {
    const size = matrix.length;
    let score = 0;
    for (let pass = 0; pass < 2; pass++) for (let line = 0; line < size; line++) {
      let run = 1;
      for (let i = 1; i < size; i++) {
        const current = pass ? matrix[i][line] : matrix[line][i];
        const previous = pass ? matrix[i - 1][line] : matrix[line][i - 1];
        run = current === previous ? run + 1 : 1;
        if (run === 5) score += 3; else if (run > 5) score++;
      }
    }
    for (let row = 0; row < size - 1; row++) for (let col = 0; col < size - 1; col++) if (matrix[row][col] === matrix[row + 1][col] && matrix[row][col] === matrix[row][col + 1] && matrix[row][col] === matrix[row + 1][col + 1]) score += 3;
    for (let row = 0; row < size; row++) for (let col = 0; col < size - 6; col++) {
      const a = matrix[row].slice(col, col + 7);
      if (a.join('') === 'true,false,true,true,true,false,true') score += 40;
      const b = [matrix[col][row], matrix[col + 1][row], matrix[col + 2][row], matrix[col + 3][row], matrix[col + 4][row], matrix[col + 5][row], matrix[col + 6][row]];
      if (b.join('') === 'true,false,true,true,true,false,true') score += 40;
    }
    let dark = 0;
    for (const row of matrix) for (const cell of row) dark += cell;
    return score + Math.floor(Math.abs(dark * 20 - size * size * 10) / (size * size)) * 10;
  }

  function render(element, text) {
    const bytes = utf8Bytes(text);
    let version = 0, data;
    for (let candidate = 1; candidate <= RS_BLOCKS.length; candidate++) {
      data = dataCodewords(bytes, candidate);
      if (data) { version = candidate; break; }
    }
    if (!version) throw new RangeError('URL is too long for the built-in QR encoder');
    const words = interleave(data, version);
    let best;
    for (let mask = 0; mask < 8; mask++) {
      const matrix = makeMatrix(version, words, mask);
      if (!best || penalty(matrix) < best.score) best = { matrix, score: penalty(matrix) };
    }
    const quiet = 4, modules = best.matrix.length + quiet * 2, scale = Math.max(3, Math.floor(192 / modules));
    const canvas = document.createElement('canvas');
    canvas.width = canvas.height = modules * scale;
    canvas.className = 'qr-code-canvas';
    canvas.setAttribute('role', 'img');
    canvas.setAttribute('aria-label', '当前分享链接的二维码');
    const context = canvas.getContext('2d');
    context.fillStyle = '#fff'; context.fillRect(0, 0, canvas.width, canvas.height);
    context.fillStyle = '#111';
    best.matrix.forEach((row, y) => row.forEach((dark, x) => { if (dark) context.fillRect((x + quiet) * scale, (y + quiet) * scale, scale, scale); }));
    element.replaceChildren(canvas);
  }

  return { render };
})();
