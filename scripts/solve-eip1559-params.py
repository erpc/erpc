import json, urllib.request
UA={"content-type":"application/json","user-agent":"Mozilla/5.0 (research)"}
def rpc(u,m,p):
    r=urllib.request.Request(u,method="POST",data=json.dumps({"jsonrpc":"2.0","id":1,"method":m,"params":p}).encode(),headers=UA)
    return json.load(urllib.request.urlopen(r,timeout=25)).get("result")
def blocks(urls,n=40):
    for u in urls:
        try:
            head=int(rpc(u,"eth_blockNumber",[]),16)
            out=[rpc(u,"eth_getBlockByNumber",[hex(h),False]) for h in range(head-n,head)]
            out=[b for b in out if b]
            if len(out)>2: return out
        except Exception: continue
    raise RuntimeError("fetch failed")
def nbf(p,el,dn,floor):
    limit=int(p["gasLimit"],16); used=int(p["gasUsed"],16); base=int(p.get("baseFeePerGas","0x0"),16)
    t=limit//el
    if t==0: return None
    if used==t: v=base
    elif used>t:
        d=base*(used-t)//t//dn; v=base+max(d,1)
    else:
        d=base*(t-used)//t//dn; v=max(base-d,0)
    return max(v,floor)
def solve(name,urls):
    try: bs=blocks(urls)
    except Exception as e: print(f"  {name:<10} FAILED {e}"); return
    pairs=[(bs[i],bs[i+1]) for i in range(len(bs)-1) if "baseFeePerGas" in bs[i] and "baseFeePerGas" in bs[i+1]]
    if not pairs: print(f"  {name:<10} no baseFeePerGas → N/A"); return
    fees=[int(b["baseFeePerGas"],16) for b in bs if "baseFeePerGas" in b]
    floors=sorted({0, min(fees)})
    best=[]
    for el in range(1,17):
        for dn in [1,2,4,6,8,10,12,16,20,25,32,50,64,100,125,128,250,256,500,1000,1024,2048]:
            for fl in floors:
                if all(nbf(p,el,dn,fl)==int(c["baseFeePerGas"],16) for p,c in pairs):
                    best.append((el,dn,fl))
    varying = len(set(fees))>1
    print(f"  {name:<10} pairs={len(pairs)} distinct_fees={len(set(fees))} min_fee={min(fees)}")
    if best:
        print(f"     FITS (elasticity, denominator, floor): {best[:6]}")
    else:
        print(f"     no (el,dn,floor) in the search space explains it → NOT derivable from the parent")
for name,urls in [
 ("mainnet",["https://ethereum-rpc.publicnode.com"]),
 ("polygon",["https://polygon-bor-rpc.publicnode.com"]),
 ("base",["https://base-rpc.publicnode.com"]),
 ("arbitrum",["https://arbitrum-one-rpc.publicnode.com"]),
 ("hyperevm",["https://rpc.hyperliquid.xyz/evm"]),
]: solve(name,urls)
