package mcp

import (
	"fmt"
	"testing"
	"time"

	"github.com/fastschema/qjs"
)

func eval(rt *qjs.Runtime, code string, flags ...qjs.EvalOptionFunc) (any, error) {
	opts := append([]qjs.EvalOptionFunc{qjs.Code(code)}, flags...)
	return rt.Eval("code.js", opts...)
}

func TestCodeMode_ColdStart(t *testing.T) {
	start := time.Now()
	rt, err := qjs.New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer rt.Close()

	result, err := eval(rt, `1 + 1`)
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	elapsed := time.Since(start)
	t.Logf("Cold start + eval: %v (result: %v)", elapsed, result)
}

func TestCodeMode_WarmEval(t *testing.T) {
	rt, err := qjs.New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer rt.Close()
	eval(rt, `1`)

	start := time.Now()
	result, err := eval(rt, `
		const data = [1, 2, 3, 4, 5];
		const sum = data.reduce((a, b) => a + b, 0);
		sum * 2;
	`)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	t.Logf("Warm eval: %v (result: %v)", elapsed, result)
}

func TestCodeMode_SyncGoFunction(t *testing.T) {
	rt, err := qjs.New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer rt.Close()

	callCount := 0
	ctx := rt.Context()
	ctx.SetFunc("callTool", func(this *qjs.This) (*qjs.Value, error) {
		callCount++
		return ctx.ParseJSON(`{"status":"ok"}`), nil
	})

	start := time.Now()
	result, err := eval(rt, `
		const r1 = callTool();
		const r2 = callTool();
		JSON.stringify({ calls: 2 });
	`)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	t.Logf("Sync Go function (2 calls): %v (result: %v, calls: %d)", elapsed, result, callCount)
}

func TestCodeMode_AsyncAwait(t *testing.T) {
	rt, err := qjs.New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer rt.Close()

	callCount := 0
	ctx := rt.Context()
	ctx.SetAsyncFunc("fetchData", func(this *qjs.This) {
		callCount++
		this.Promise().Resolve(ctx.ParseJSON(`{"data":"hello","count":42}`))
	})

	start := time.Now()
	result, err := eval(rt, `
		async function main() {
			const r = await fetchData();
			return JSON.stringify(r);
		}
		await main();
	`, qjs.FlagAsync())
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	t.Logf("Async/await: %v (result: %v, calls: %d)", elapsed, result, callCount)
}

func TestCodeMode_MultipleAsyncCalls(t *testing.T) {
	rt, err := qjs.New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer rt.Close()

	callCount := 0
	ctx := rt.Context()
	ctx.SetAsyncFunc("callMCPTool", func(this *qjs.This) {
		callCount++
		this.Promise().Resolve(ctx.ParseJSON(fmt.Sprintf(`{"call":%d}`, callCount)))
	})

	start := time.Now()
	result, err := eval(rt, `
		async function main() {
			const r1 = await callMCPTool("weather");
			const r2 = await callMCPTool("calendar");
			const r3 = await callMCPTool("email");
			return JSON.stringify({ results: [r1, r2, r3] });
		}
		await main();
	`, qjs.FlagAsync())
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	t.Logf("3 async tool calls: %v (result: %v, calls: %d)", elapsed, result, callCount)
}

func TestCodeMode_ES2023Features(t *testing.T) {
	rt, err := qjs.New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer rt.Close()

	result, err := eval(rt, "const obj={a:{b:{c:42}}}; const v=obj?.a?.b?.c??0; const [f,...r]=[1,2,3]; `${v},${f},${r.length}`")
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	t.Logf("ES2023: %v", result)
}

func TestCodeMode_BothSyncAndAsync(t *testing.T) {
	rt, err := qjs.New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer rt.Close()

	ctx := rt.Context()

	// Sync function
	ctx.SetFunc("syncTool", func(this *qjs.This) (*qjs.Value, error) {
		return ctx.ParseJSON(`{"sync":true}`), nil
	})

	// Async function
	ctx.SetAsyncFunc("asyncTool", func(this *qjs.This) {
		this.Promise().Resolve(ctx.ParseJSON(`{"async":true}`))
	})

	// LLM can use EITHER style
	result, err := eval(rt, `
		async function main() {
			const s = syncTool();
			const a = await asyncTool();
			return JSON.stringify({ sync: s, async: a });
		}
		await main();
	`, qjs.FlagAsync())
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	t.Logf("Both sync + async: %v", result)
}

func BenchmarkCodeMode_ColdStart(b *testing.B) {
	for i := 0; i < b.N; i++ {
		rt, err := qjs.New()
		if err != nil {
			b.Fatalf("New: %v", err)
		}
		eval(rt, `1 + 1`)
		rt.Close()
	}
}

func BenchmarkCodeMode_WarmEval(b *testing.B) {
	rt, err := qjs.New()
	if err != nil {
		b.Fatalf("New: %v", err)
	}
	defer rt.Close()
	eval(rt, `1`)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		eval(rt, `[1,2,3].map(n => n * 2).reduce((a,b) => a+b, 0)`)
	}
}

func BenchmarkCodeMode_AsyncCall(b *testing.B) {
	rt, err := qjs.New()
	if err != nil {
		b.Fatalf("New: %v", err)
	}
	defer rt.Close()

	ctx := rt.Context()
	ctx.SetAsyncFunc("noop", func(this *qjs.This) {
		this.Promise().Resolve(ctx.ParseJSON(`{"ok":true}`))
	})

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		eval(rt, `
			async function run() { return await noop(); }
			await run();
		`, qjs.FlagAsync())
	}
}
