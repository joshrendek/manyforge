// Compiler-resolved packaged declarations; no bundled chunk names enter the snapshot.
const path = require('node:path');
const ts = require(process.argv[2]);
const root = path.resolve(process.argv[3]);
const entries = ['index', 'public', 'server'].flatMap(name => ['ts', 'cts'].map(ext => ({name: `${ext === 'ts' ? 'import' : 'require'}:${name}`, file: path.join(root, 'dist', `${name}.d.${ext}`)})));
const program = ts.createProgram(entries.map(e => e.file), {strict: true, target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.NodeNext, moduleResolution: ts.ModuleResolutionKind.NodeNext, skipLibCheck: true, noEmit: true});
const checker = program.getTypeChecker();
const diagnostics = [...program.getOptionsDiagnostics(), ...program.getSyntacticDiagnostics(), ...program.getSemanticDiagnostics()];
if (diagnostics.length) throw Error(ts.formatDiagnosticsWithColorAndContext(diagnostics, {getCanonicalFileName: x => x, getCurrentDirectory: () => root, getNewLine: () => '\n'}));
const symbols = {}, queue = [], queued = new Map();
let namespace = '';
function resolve(symbol) { return symbol.flags & ts.SymbolFlags.Alias ? checker.getAliasedSymbol(symbol) : symbol; }
function local(symbol) { return symbol?.declarations?.some(d => path.resolve(d.getSourceFile().fileName).startsWith(root + path.sep)); }
function enqueue(symbol) {
  if (!symbol) return;
  symbol = resolve(symbol);
  if (!local(symbol) || symbol.name.startsWith('__')) return;
  if (!symbol.declarations.some(d => ts.isClassDeclaration(d) || ts.isInterfaceDeclaration(d) || ts.isTypeAliasDeclaration(d) || ts.isEnumDeclaration(d))) return;
  if (!queued.has(namespace)) queued.set(namespace, new Set());
  if (queued.get(namespace).has(symbol)) return;
  queued.get(namespace).add(symbol); queue.push({symbol, namespace});
}
function track(type, seen = new Set()) {
  if (!type || seen.has(type)) return;
  seen.add(type); enqueue(type.aliasSymbol); enqueue(type.symbol);
  for (const t of type.aliasTypeArguments || []) track(t, seen);
  for (const t of type.types || []) track(t, seen);
  if (type.objectFlags & ts.ObjectFlags.Reference) for (const t of checker.getTypeArguments(type)) track(t, seen);
}
function text(type) {
  track(type);
  return checker.typeToString(type, undefined, ts.TypeFormatFlags.NoTruncation)
    .replace(/import\("[^"]+"\)\./g, '');
}
function signature(sig) {
  return {params: sig.parameters.map((p, index) => {
    const d = p.valueDeclaration || p.declarations?.[0] || sig.declaration;
    return {name: String(index), type: text(checker.getTypeOfSymbolAtLocation(p, d)), required: !(p.flags & ts.SymbolFlags.Optional) && !d.questionToken && !d.initializer && !d.dotDotDotToken, rest: !!d.dotDotDotToken};
  }), returns: text(checker.getReturnTypeOfSignature(sig)), type_parameters: (sig.typeParameters || []).map(p => ({name: p.symbol.name, constraint: p.getConstraint() ? text(p.getConstraint()) : null, default: p.getDefault() ? text(p.getDefault()) : null}))};
}
function privateMember(symbol) { return symbol.declarations?.some(d => d.modifiers?.some(m => m.kind === ts.SyntaxKind.PrivateKeyword) || ts.isPrivateIdentifier(d.name || {})); }
function member(symbol, location) {
  const type = checker.getTypeOfSymbolAtLocation(symbol, symbol.valueDeclaration || symbol.declarations?.[0] || location);
  const calls = type.getCallSignatures();
  const readonly = !!symbol.declarations?.some(d => d.modifiers?.some(m => m.kind === ts.SyntaxKind.ReadonlyKeyword));
  if (calls.length) return {kind: 'method', signatures: calls.map(signature)};
  return {kind: 'field', type: text(type), required: !(symbol.flags & ts.SymbolFlags.Optional), readonly};
}
function object(type, declaration, kind) {
  const members = {};
  for (const symbol of checker.getPropertiesOfType(type)) {
    if (privateMember(symbol)) continue;
    members[symbol.name] = member(symbol, declaration);
    if (kind !== 'class' && members[symbol.name].kind === 'method') members[symbol.name].required = !(symbol.flags & ts.SymbolFlags.Optional);
    // Class properties can be added without forcing callers to implement them.
    if (kind === 'class') members[symbol.name].required = false;
  }
  const out = {kind, members};
  const calls = type.getCallSignatures();
  if (calls.length) out.signatures = calls.map(signature);
  const indexes = checker.getIndexInfosOfType(type);
  if (indexes.length) out.indexes = indexes.map(i => ({key: text(i.keyType), value: text(i.type), readonly: i.isReadonly}));
  return out;
}
function describe(symbol) {
  const declaration = symbol.declarations[0];
  const type = checker.getDeclaredTypeOfSymbol(symbol);
  if (symbol.flags & (ts.SymbolFlags.Class | ts.SymbolFlags.Interface)) {
    const out = object(type, declaration, symbol.flags & ts.SymbolFlags.Class ? 'class' : 'interface');
    out.type_parameters = (type.typeParameters || []).map(p => ({name: p.symbol.name, constraint: p.getConstraint() ? text(p.getConstraint()) : null, default: p.getDefault() ? text(p.getDefault()) : null}));
    out.bases = (type.getBaseTypes?.() || []).map(text).sort();
    if (symbol.flags & ts.SymbolFlags.Class) {
      const ctor = checker.getTypeOfSymbolAtLocation(symbol, declaration);
      out.signatures = ctor.getConstructSignatures().map(signature);
      for (const staticMember of checker.getPropertiesOfType(ctor)) if (staticMember.name !== 'prototype' && !privateMember(staticMember)) out.members['static:' + staticMember.name] = member(staticMember, declaration);
    }
    return out;
  }
  if (symbol.flags & ts.SymbolFlags.TypeAlias) {
    // Resolve aliases to their real compiler type, rather than returning the alias name itself.
    if (type.flags & ts.TypeFlags.Object && !(type.objectFlags & ts.ObjectFlags.Reference)) return object(type, declaration, 'type');
    const expanded = checker.typeToString(type, declaration, ts.TypeFormatFlags.NoTruncation | ts.TypeFormatFlags.InTypeAlias).replace(/import\("[^"]+"\)\./g, '');
    track(type);
    return {kind: 'type', type: expanded};
  }
  return member(symbol, declaration);
}
for (const entry of entries) {
  namespace = entry.name;
  const file = program.getSourceFile(entry.file);
  if (!file) throw Error(`Missing packaged declaration entrypoint ${entry.name}`);
  const module = checker.getSymbolAtLocation(file);
  if (!module) throw Error(`Missing module exports ${entry.name}`);
  const exports = checker.getExportsOfModule(module);
  if (!exports.length) throw Error(`Empty exports ${entry.name}`);
  for (const exported of exports) {
    const symbol = resolve(exported);
    enqueue(symbol);
    let node;
    if (symbol.flags & (ts.SymbolFlags.Class | ts.SymbolFlags.Interface | ts.SymbolFlags.TypeAlias | ts.SymbolFlags.Enum)) node = {kind: 'export', target: (symbol.flags & ts.SymbolFlags.Class ? 'class:' : 'type:') + symbol.name};
    else node = member(symbol, file);
    if (exported.name === 'VERSION' || exported.name === 'RELEASE_VERSION') node = {kind: 'constant', type: 'string'};
    symbols[entry.name + '.' + exported.name] = node;
  }
}
for (let index = 0; index < queue.length; index++) {
  const item = queue[index], symbol = item.symbol;
  namespace = item.namespace;
  const key = namespace + '.' + (symbol.flags & ts.SymbolFlags.Class ? 'class:' : 'type:') + symbol.name, node = describe(symbol);
  if (symbols[key] && JSON.stringify(symbols[key]) !== JSON.stringify(node)) throw Error(`Conflicting public declaration identity ${key}`);
  symbols[key] = node;
}
process.stdout.write(JSON.stringify({symbols}));
