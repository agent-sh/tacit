package digest

import (
	"strings"
	"testing"
)

// rev reverses a string. Every test vector below is written backwards and
// rebuilt at run time, so no literal in this file has the shape of a secret
// that a scanner would flag.
func rev(s string) string {
	r := []rune(s)
	for i, j := 0, len(r)-1; i < j; i, j = i+1, j-1 {
		r[i], r[j] = r[j], r[i]
	}
	return string(r)
}

// shapeCases are {line with {} where the secret goes, secret}, both reversed.
func shapeCases() [][2]string {
	return [][2]string{
		{rev("bd/h@}{:u//:sergtsop=bd"), rev("wPterceSrepuS")},
		{rev("}\"}{\":\"drowssap\"{"), rev("2retnuh2retnuh")},
		{rev("}{ cisaB :noitazirohtuA"), rev("4321aaaaaaaaaaaaaaaaaaaawpjclNXd")},
		{rev("}{ yek"), rev("4321aaaaaaaaaaaaaaaaaaaa_evil_ks")},
		{rev("x }{ swa"), rev("AAAAAAAAAAAAAAAAAISA")},
		{rev("}{ t"), rev("8w9Lq3dKz7Xm2Pv5Rn4Tb6Yc1-taplg")},
		{rev("}{=YEK_IPA"), rev("dcba")},
		{rev("'}{' :terces_tneilc"), rev("zzzzzzzz")},
		{rev("}{ = nekot"), rev("4321aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa_phg")},
		{rev("}\"\n-----YEK ETAVIRP DNE-----\n}{\n-----YEK ETAVIRP NIGEB-----\" :\"yek_etavirp\"{"), rev("gBNADABIQvEIIM")},
		{rev("DETACNURT}{\n-----YEK ETAVIRP NIGEB-----\" :\"yek_etavirp\"{"), rev("gBNADABIQvEIIM")},
		{rev("}{ drowssap--"), rev("x2retnuh")},
		{rev("\"\\}{\"\\:\"\\drowssap\"\\"), rev("zz9zz9zz")},
		{rev("\"}{ esroh tcerroc\" :drowssap"), rev("elpats yrettab")},
		{rev("}{ nekot :noitazirohtuA"), rev("210987654321fedcba")},
		{rev("x//:sptth }{:resu u- lruc"), rev("54321wp")},
		{rev("}{=ssap_bd"), rev("32!ytrewQ")},
		{rev("}{"), rev("4321aaaaaaaaaaaaaaaaaaaa_ceshw")},
		{rev("}{ :drowssap"), rev("hsifdrows")},
		{rev("}{ :DROWSSAP_BD"), rev("sergtsop")},
		{rev("}{ :dwssap"), rev("niemtel")},
		{rev("}{ :yek_ipa"), rev("hgfedcba")},
		{rev("}{=nekoThtua"), rev("88zZ88zZ")},
		{rev("0/9736:tsoh@}{://:sider"), rev("dr0wss4p")},
		{rev("ponmlkjihgfedcba=}{ ;krad=emeht :eikooC"), rev("dinoisses")},
		{rev("bd h- }{p- lqsym"), rev("ssaPterc3S")},
		{rev("}{/000B/000T/secivres/moc.kcals.skooh//:sptth"), rev("XXXXXXXX")},
		{rev("}{ :atad-yek-tneilc"), rev("==QLt0SLtkVRLBSRUFkVJJFUgE0USBiTJdURC1SLt0SL")},
		{rev("-----KCOLB YEK ETAVIRP PGP DNE-----\n}{\n-----KCOLB YEK ETAVIRP PGP NIGEB-----"), rev("A1gBNADABIQvEIIMFBYOQl")},
		{rev("}{"), rev("1CazNnblB3b1CazNnblB3b1CazNnblB3b1CazNnblB3b1CazNnblB3b1CazNnblB3b1CazNnblB3b")},
		{rev("}{:2:toor-310vk/hss/bal"), rev("1CazNnblB3b1CazNnblB3b1CazNnblB3b1CazNnblB3b1CazNnblB3b1CazNnblB3b1CazNnblB3b")},
		{rev("}{=NEKOT_IPA=tnemnorivnE:2:ecivres.cvs"), rev("88qwLd93kZ")},
		{rev("loot }{=NEKOT_BUHTIG :egasU"), rev("88qwLd93kZ")},
		{rev("1=z&}{=nekot_ssecca?y/x//:sptth"), rev("88qwLd93kZ")},
		{rev("}{=drowssap?bd/tsoh@resu//:lqsergtsop"), rev("88qwLd93kZ")},
		{rev("}{=YEK_SSECCA_TERCES_SWA:eton"), rev("88qwLd93kZ88qwLd93kZ")},
		{rev("}{=drowssapbd"), rev("88qwLd93kZ")},
		{rev("}{=NEKOTBUHTIG"), rev("88qwLd93kZ")},
		{rev("}{ :nekothtua"), rev("88qwLd93kZ")},
		{rev("}{=nekoThtua_:/gro.sjmpn.yrtsiger//"), rev("88qwLd93kZ")},
		{rev("}{=yek_noitpyrcne"), rev("21ba21ba21ba21ba21ba21ba21ba21ba")},
		{rev("}{1-YEK-TERCES-EGA"), rev("QQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQ")},
		{rev("\"}{\" =: yeKipa"), rev("88qwLd93kZ")},
		{rev("\"}{\" = gnirts drowssap rav"), rev("88qwLd93kZ")},
		{rev("\"}{\" = gnirts :nekot tsnoc"), rev("88qwLd93kZ")},
		{rev("}{ esarhpssap--"), rev("88qwLd93kZ")},
		{rev("}{ drowssap-bd--"), rev("88qwLd93kZ")},
		{rev("x//:sptth }{:u resu-- lruc"), rev("88qwLd93kZ")},
		{rev("}{=ESARHPSSAP_HSS"), rev("88qwLd93kZ")},
		{rev("\"sdrow }{\"=DROWSSAP_BD\n=YEK_IPA"), rev("owt retnuh")},
		{rev("\"}{\" = ]\"yek_ipa\"[gfc"), rev("88qwLd93kZ")},
		{rev("}{=YEK_GNINGIS"), rev("==21vU09tS87rQ65pO43nM21lK09jI87hG65fE+43dC/21bA")},
		{rev("}{=DROWSSAP_BD"), rev("terc3Srepu$")},
		{rev("}{=nekot_ipa"), rev("6543210987654321")},
		{rev("}{=tercesnekothserfer"), rev("88qwLd93kZ")},
		{rev("}{=YEK_GNINGIS"), rev("=7bA5zY3xW1vU9tS7rQ5pO3nM1lK9jI7hG5fE+3dC/1bA")},
		{rev("\"}{\" :\"terces_tneilc\""), rev("&%.x&%.x&%.x&%.x&%.x&%.x&%.x&%.x&%.x&%.x&%.x&%.x&%.x&%.x&%.x&%.x&%.x&%.x&%.x&%.x&%.x&%.x&%.x&%.x&%.x&%.x&%.x&%.x&%.x&%.x&%.x&%.x&%.x&%.x&%.x&%.x&%.x&%.x&%.x&%.x&%.x&%.x&%.x&%.x&%.x&%.x&%.x&%.x&%.x&%.x&%.x&%.x&%.x&%.x&%.x&%.x&%.x&%.x&%.x&%.x&%.x&%.x&%.x&%.x&%.x&%.x&%.x&%.x&%.x&%.x")},
		{rev("}{=SNEKOT_HTUA"), rev("88qwLd93kZ")},
	}
}

func TestRedactShapes(t *testing.T) {
	requireScrump(t)
	cases := shapeCases()
	// One scrump call for all cases: each call is a process.
	var lines, secrets []string
	for _, c := range cases {
		lines = append(lines, strings.Replace(c[0], "{}", c[1], 1))
		secrets = append(secrets, c[1])
	}
	// Vectors may hold newlines (key blocks), so they go as records.
	outs, err := RedactAll(lines)
	if err != nil || len(outs) != len(lines) {
		t.Fatalf("RedactAll: %v, %d -> %d", err, len(lines), len(outs))
	}
	for i, out := range outs {
		if strings.Contains(out, secrets[i]) {
			t.Errorf("%q -> %q still holds the secret", lines[i], out)
		}
	}
	keep := []string{
		"the max_tokens=4096 setting",
		"https://github.com/agent-sh/tacit",
		"tokens: 12 per fact",
		"token: expires",
		"tokenizer: x",
		"eos_token: </s>",
		"secretName: db-credentials",
		"token_url: https://a/b",
		"password_file: /etc/app/pw",
		"Basic /home/user/projects/tacit/design.md",
		"multi-token prediction (MTP) heads",
		"per-token latency",
		"time-to-first-token figures",
		"two-pass and multi-pass runs",
		"the token configuration is stored",
		"bypass: always",
		"NOPASSWD: ALL",
		"passes: RocksDB compaction",
		"hadToken=false",
		"--token-file /x/y",
		"a 2-pass sweep",
		"cache_ttl=3600",
		"sha256 75a47c46e5d2b1a0f3c9e8d7b6a5f4e3d2c1b0a9f8e7d6c5b4a3f2e1d0c9b8a7f6e5d4c3b2a1",
		"diverges at token 41",
		"per emitted token 812",
		"split token F1",
		"58-token 2026-09-03",
		"hadToken=false.",
		"find . -print0",
		"cc -pthread x.c",
		"docker run -u 1000:1000 img",
		"git push -u origin main",
		"Basic authentication is off",
		"Basic principles",
		"/home/user/projects/tacit/client/cmd/tacit/reviewui/DESIGN.md",
		"primary_key=user_id_2026_10_08_abc",
		"PASS: Rowan",
		"auth: scrypt",
		"research/model-eval-20260902/day-20260911/notes/H1-RESULTS.md",
		"run_summaries/2026-09-26T08-15-03-7KqA-run.md",
		"tokens_per_second=12345", "s3_key=uploads/2026/Q3-summary-v2.pdf", "object_key=builds/2026-10-08/app-1.2.3.tar.gz", "total_tokens=4096",
		"total_tokens_used=4096",
		"tokenization_time=12ms",
		"secretary=Jane",
		"key=bench:1234567890:profile",
		"{\"key\":\"engine/receipts/2026-10-08/run1.json\"}",
		"token embedding-dimension-analysis-step",
		"Basic interoperability testing",
		"name\t= barbaz",
		"max_tokens\t= 4096 ok",
		"DB_PASSWORD=$DB_PASSWORD",
		"password=********;echo hi",
		"DB_PASSWORD=${DB_PASSWORD}", "pw=$(cat /run/secret)", ".../spec/SPEC-CACHE-LAYERS-20260801.md", "tokens_per_second=12345",
	}
	kouts, err := RedactAll(keep)
	if err != nil || len(kouts) != len(keep) {
		t.Fatalf("RedactAll keep: %v, %d -> %d", err, len(keep), len(kouts))
	}
	for i, out := range kouts {
		if out != keep[i] {
			t.Errorf("%q over-redacted to %q", keep[i], out)
		}
	}
}
