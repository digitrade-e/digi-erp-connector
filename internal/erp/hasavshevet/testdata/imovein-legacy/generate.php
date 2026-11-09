<?php
// Generates the legacy-layout IMOVEIN goldens (DIG-1297) by RUNNING the legacy
// writer, not a copy of it: the five functions below are lifted by name out of
// the old app's backend/app/new-api/sync/global_api.php at run time and called
// unchanged on the fixtures in cases.json.
//
//   docker run --rm -v <old app>/backend/app/new-api/sync:/legacy:ro \
//     -v "$PWD":/out -w /out php:8.3-cli php generate.php /legacy/global_api.php
//
// Writes <case>.doc (the legacy's <prefix>IMOVEIN_<n>.doc), IMOVEIN.prm (the
// legacy's per-order .prm; identical for every case, which it checks),
// names.txt (the file name the legacy gives each case) and source.txt (sha256
// of global_api.php and the PHP version). Regenerate, never hand-edit.

$want = ['WriteDocSpaces', 'getHerpOrderConstantLengths', 'prepareForHerpDoc', 'CreatePrmDoc', 'CreateMainDoc'];
$srcPath = $argv[1] ?? '';
$src = @file_get_contents($srcPath);
if ($src === false) {
    fwrite(STDERR, "usage: php generate.php <path to global_api.php>\n");
    exit(2);
}

$tokens = token_get_all($src);
$methods = [];
$n = count($tokens);
for ($i = 0; $i < $n; $i++) {
    if (!is_array($tokens[$i]) || $tokens[$i][0] !== T_FUNCTION) {
        continue;
    }
    $j = $i + 1;
    while ($j < $n && is_array($tokens[$j]) && $tokens[$j][0] === T_WHITESPACE) {
        $j++;
    }
    if (!is_array($tokens[$j]) || $tokens[$j][0] !== T_STRING || !in_array($tokens[$j][1], $want, true)) {
        continue;
    }
    $name = $tokens[$j][1];
    if (isset($methods[$name])) {
        fwrite(STDERR, "function $name is defined twice\n");
        exit(1);
    }
    $code = '';
    $depth = 0;
    for ($k = $i; $k < $n; $k++) {
        $t = $tokens[$k];
        $text = is_array($t) ? $t[1] : $t;
        $code .= $text;
        if ($text === '{' || (is_array($t) && ($t[0] === T_CURLY_OPEN || $t[0] === T_DOLLAR_OPEN_CURLY_BRACES))) {
            $depth++;
        } elseif ($text === '}') {
            $depth--;
            if ($depth === 0) {
                break;
            }
        }
    }
    $methods[$name] = $code;
}
foreach ($want as $name) {
    if (!isset($methods[$name])) {
        fwrite(STDERR, "function $name not found\n");
        exit(1);
    }
}

eval('class LegacyHerpWriter { public $herpInnerPath; ' . implode("\n", $methods) . ' }');

$here = __DIR__;
$cases = json_decode(file_get_contents("$here/cases.json"), true, 512, JSON_THROW_ON_ERROR);
$work = sys_get_temp_dir() . '/legacy-imovein-' . getmypid();
mkdir($work);

$w = new LegacyHerpWriter();
$w->herpInnerPath = $work . '/';
$prm = null;
$names = '';
foreach ($cases as $case) {
    $stock = (object) $case['stock'];
    $moves = array_map(fn ($m) => (object) $m, $case['moves']);
    $id = $stock->MyId;
    $addTMP = ($stock->Status == 0) ? 'TMP_' : ''; // prepareOrderObj :6521
    mkdir("$work/$id");
    $lengths = $w->getHerpOrderConstantLengths();
    $rows = $w->prepareForHerpDoc($stock, $moves, $lengths);
    $w->CreatePrmDoc($lengths, $id, $addTMP);
    $w->CreateMainDoc($rows, $id, $addTMP);

    $base = $addTMP . 'IMOVEIN_' . $id;
    copy("$work/$id/$base.doc", "$here/{$case['name']}.doc");
    $p = file_get_contents("$work/$id/$base.prm");
    if ($prm !== null && $p !== $prm) {
        fwrite(STDERR, "the .prm differs between cases\n");
        exit(1);
    }
    $prm = $p;
    $names .= $case['name'] . ' ' . $base . ".doc\n";
}
file_put_contents("$here/IMOVEIN.prm", $prm);
file_put_contents("$here/names.txt", $names);
file_put_contents("$here/source.txt", 'global_api.php sha256 ' . hash_file('sha256', $srcPath) . "\nphp " . PHP_VERSION . "\n");
echo "ok: " . count($cases) . " cases\n";
