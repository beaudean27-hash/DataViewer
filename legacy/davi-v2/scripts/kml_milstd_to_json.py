#!/usr/bin/env python3
"""Convert a MIL-STD-2525 KMZ/KML (TrackServer / Google Earth Pro export — the
flavour where each Placemark's <description> embeds an
``<img src="resources/thumb/<SIDC>.png" alt="<SIDC>">`` thumbnail) into the
Base_Graphics.json record schema for loading into Kibana / DataViewer.

Each Placemark becomes one or more records (MultiGeometry is unwrapped) with
the actual SIDC carried into ``SymbolCode``. The HTML attribute table is
flattened into ``Comment`` so battle dim / function / hostility text is
preserved.

Usage:
    python3 kml_milstd_to_json.py <input.kmz|.kml> [<output.json>] [--ndjson]
                                                   [--index NAME]
"""
from __future__ import annotations
import argparse
import datetime as _dt
import json
import re
import sys
import uuid
import xml.etree.ElementTree as ET
import zipfile

KML_NS = "http://www.opengis.net/kml/2.2"
NS = {"k": KML_NS}

# 15-char SIDC (allow '-', '*', alnum)
SIDC_RE = re.compile(r'\b([A-Z0-9\*\-]{15})\b')


def _strip_ns(tag: str) -> str:
    return tag.split("}", 1)[-1] if "}" in tag else tag


def _parse_html_attrs(html: str) -> dict[str, str]:
    if not html:
        return {}
    pairs = re.findall(
        r"<(?:th|td)[^>]*>\s*<b[^>]*>\s*(.*?)\s*:\s*</b>\s*</(?:th|td)>\s*<td[^>]*>\s*(.*?)\s*</td>",
        html, flags=re.IGNORECASE | re.DOTALL,
    )
    out: dict[str, str] = {}
    for k, v in pairs:
        k = re.sub(r"<[^>]+>", "", k).strip()
        v = re.sub(r"<[^>]+>", "", v).strip()
        if k:
            out[k] = v
    return out


def _extract_sidc(desc: str) -> str | None:
    """Find the SIDC. First try ``<img alt="SIDC">``; fall back to a bare
    15-char token in the text."""
    if not desc:
        return None
    m = re.search(r'<img[^>]*\balt\s*=\s*"([^"]{15})"', desc, flags=re.IGNORECASE)
    if m:
        return m.group(1).upper()
    m = re.search(r'thumb/([A-Z0-9\*\-]{15})\.png', desc, flags=re.IGNORECASE)
    if m:
        return m.group(1).upper()
    m = SIDC_RE.search(desc)
    return m.group(1).upper() if m else None


def _parse_coordinates(text: str) -> list[tuple[float, float]]:
    pts: list[tuple[float, float]] = []
    if not text:
        return pts
    for tok in text.split():
        parts = tok.split(",")
        if len(parts) < 2:
            continue
        try:
            lon = float(parts[0]); lat = float(parts[1])
        except ValueError:
            continue
        if -180.0 <= lon <= 180.0 and -90.0 <= lat <= 90.0:
            pts.append((lat, lon))
    return pts


def _centroid(points):
    if not points:
        return None, None
    return (sum(p[0] for p in points) / len(points),
            sum(p[1] for p in points) / len(points))


def _walk_geometries(el, into):
    tag = _strip_ns(el.tag)
    if tag == "Point":
        pts = _parse_coordinates(el.findtext("k:coordinates", default="", namespaces=NS))
        if pts:
            into.append(("Point", pts[:1]))
    elif tag == "LineString":
        pts = _parse_coordinates(el.findtext("k:coordinates", default="", namespaces=NS))
        if len(pts) >= 2:
            into.append(("Line", pts))
    elif tag == "LinearRing":
        pts = _parse_coordinates(el.findtext("k:coordinates", default="", namespaces=NS))
        if len(pts) >= 3:
            into.append(("Area", pts))
    elif tag == "Polygon":
        outer = el.find("k:outerBoundaryIs/k:LinearRing/k:coordinates", NS)
        if outer is not None:
            pts = _parse_coordinates(outer.text or "")
            if len(pts) >= 3:
                into.append(("Area", pts))
    elif tag == "MultiGeometry":
        for child in el:
            _walk_geometries(child, into)


def _sym_type_for(sidc: str, kind: str) -> str:
    """Pick a Systematic-style SymbolType label from the SIDC scheme."""
    scheme = (sidc or "").upper()[:1]
    if scheme == "G":
        # G-scheme = tactical graphics. BoundaryLine prefix is GFGPGLB.
        if sidc[:7].upper().startswith(("GFGPGLB", "GHGPGLB", "GNGPGLB", "GUGPGLB")):
            return "BoundaryLine"
        return "TacticalGraphic"
    if scheme == "S":
        return "Unit"
    if scheme == "W":
        return "Weather"
    if scheme == "I":
        return "SignalsIntelligence"
    if scheme == "O":
        return "StabilityOperations"
    return "Symbol"


def placemark_to_records(pm, folder: str, dtg: str) -> list[dict]:
    name = (pm.findtext("k:name", default="", namespaces=NS) or "").strip()
    desc = pm.findtext("k:description", default="", namespaces=NS) or ""
    attrs = _parse_html_attrs(desc)
    sidc = _extract_sidc(desc) or "SNGPU-------***"

    geometries: list = []
    for child in pm:
        if _strip_ns(child.tag) in {"Point", "LineString", "LinearRing", "Polygon", "MultiGeometry"}:
            _walk_geometries(child, geometries)

    records: list[dict] = []
    for kind, pts in geometries:
        lat, lon = _centroid(pts)
        sym_type = _sym_type_for(sidc, kind)
        comment_bits = [f"{k}: {v}" for k, v in attrs.items() if v]
        rec = {
            "Id": str(uuid.uuid4()),
            "SymbolType": sym_type,
            "Name": name or attrs.get("Function") or sym_type,
            "AbbreviatedName": "",
            "LeftOrganisation": "",
            "RightOrganisation": "",
            "Occupant": "",
            "SymbolCode": sidc,
            "OperationalStatus": "",
            "GeopoliticalAffiliation": "",
            "Priority": "Medium",
            "LocationType": kind,
            "Latitude": lat,
            "Longitude": lon,
            "StaffComments": "",
            "Comment": " | ".join(comment_bits),
            "MessageDTG": dtg,
            "SigAct": "",
            "UnitPosture": "",
            "UnitStrength": "",
            "Category": folder or sym_type,
            "Location": {
                "@xsi:type": kind,
                "Points": {
                    "Point": [{"Latitude": p[0], "Longitude": p[1]} for p in pts]
                },
            },
        }
        records.append(rec)
    return records


def collect_records(root):
    dtg = _dt.datetime.now(_dt.timezone.utc).strftime("%Y-%m-%dT%H:%M:%S.000Z")
    out: list[dict] = []

    def walk(el, folder):
        tag = _strip_ns(el.tag)
        if tag == "Folder":
            nm = el.findtext("k:name", default="", namespaces=NS) or folder
            folder = nm.strip()
        if tag == "Placemark":
            out.extend(placemark_to_records(el, folder, dtg))
            return
        for child in el:
            walk(child, folder)

    walk(root, "")
    return out


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("input")
    ap.add_argument("output", nargs="?", default=None)
    ap.add_argument("--ndjson", action="store_true")
    ap.add_argument("--index", default="milstd_records")
    args = ap.parse_args()

    if args.input.lower().endswith(".kmz"):
        with zipfile.ZipFile(args.input) as zf:
            kml_name = next((n for n in zf.namelist() if n.lower().endswith(".kml")), None)
            if not kml_name:
                print(f"error: no .kml in {args.input}", file=sys.stderr); return 2
            with zf.open(kml_name) as fh:
                tree = ET.parse(fh)
    else:
        tree = ET.parse(args.input)

    records = collect_records(tree.getroot())
    out_path = args.output or (args.input.rsplit(".", 1)[0] + ".json")

    if args.ndjson:
        with open(out_path, "w", encoding="utf-8") as fh:
            for rec in records:
                fh.write(json.dumps({"index": {"_index": args.index, "_id": rec["Id"]}}) + "\n")
                fh.write(json.dumps(rec, ensure_ascii=False) + "\n")
    else:
        with open(out_path, "w", encoding="utf-8") as fh:
            json.dump(records, fh, ensure_ascii=False, indent=2)

    by_kind: dict[str, int] = {}; by_type: dict[str, int] = {}; by_aff: dict[str, int] = {}
    for r in records:
        by_kind[r["LocationType"]] = by_kind.get(r["LocationType"], 0) + 1
        by_type[r["SymbolType"]]   = by_type.get(r["SymbolType"], 0) + 1
        aff = (r["SymbolCode"] or "")[1:2] or "?"
        by_aff[aff] = by_aff.get(aff, 0) + 1
    print(f"Wrote {len(records)} records → {out_path}", file=sys.stderr)
    print(f"  LocationType:  {by_kind}", file=sys.stderr)
    print(f"  SymbolType:    {by_type}", file=sys.stderr)
    print(f"  Affiliation:   {by_aff}", file=sys.stderr)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
