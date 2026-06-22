#!/usr/bin/env python3
"""Convert a Systematic SitaWare/CPCE Situation Layer File (.slf) to a JSON
array of per-symbol records suitable for ingest into Elasticsearch/Kibana.

Each Symbol in the SLF becomes a single top-level record, preserving its
MIL-STD-2525 SymbolCode and original geometry so the DataViewer can render
it correctly. The output schema mirrors TF_WEED_COP_records.json /
NTC_GCM_records.json (a flat record with a nested ``Location`` block).

Usage:
    python3 slf_to_json.py <input.slf> [<output.json>]
    python3 slf_to_json.py <input.slf> --ndjson [<output.ndjson>] [--index NAME]
"""

from __future__ import annotations

import argparse
import json
import os
import sys
import xml.etree.ElementTree as ET
from typing import Any

# Namespaces used in the SLF XML.
KML_NS = "http://schemas.systematic.com/2011/products/layer-definition-v4"
XSI_NS = "http://www.w3.org/2001/XMLSchema-instance"
NS = {"k": KML_NS, "xsi": XSI_NS}

# Fully-qualified xsi:type attribute key.
XSI_TYPE = f"{{{XSI_NS}}}type"


def k(tag: str) -> str:
    """Qualify a tag with the SLF namespace."""
    return f"{{{KML_NS}}}{tag}"


def find_text(el: ET.Element | None, *path: str, default: str = "") -> str:
    """Find the text content at a chain of child tags, or return ``default``."""
    if el is None:
        return default
    cur: ET.Element | None = el
    for tag in path:
        if cur is None:
            return default
        cur = cur.find(k(tag))
    if cur is None or cur.text is None:
        return default
    return cur.text.strip()


def point_dict(el: ET.Element) -> dict[str, str]:
    """Convert a <Point>/<Arrowhead>/<Endpoint>/<StartPoint> into a JSON dict."""
    return {
        "Latitude": find_text(el, "Latitude"),
        "Longitude": find_text(el, "Longitude"),
    }


def collect_points(el: ET.Element) -> list[dict[str, str]]:
    """Return all <Point> children under ``el``'s <Points> wrapper."""
    pts_wrap = el.find(k("Points"))
    if pts_wrap is None:
        return []
    return [point_dict(p) for p in pts_wrap.findall(k("Point"))]


def location_to_dict(loc: ET.Element) -> tuple[dict[str, Any], list[tuple[float, float]]]:
    """Convert a <Location> element to a JSON dict and a list of (lat, lon)
    points used for centroid calculation."""
    loc_type = loc.get(XSI_TYPE, "").split(":")[-1]
    out: dict[str, Any] = {"@xsi:type": loc_type}
    pts: list[tuple[float, float]] = []

    def _push(lat_s: str, lon_s: str) -> None:
        try:
            pts.append((float(lat_s), float(lon_s)))
        except (TypeError, ValueError):
            pass

    if loc_type in ("Area", "Line", "PolyPoint"):
        points = collect_points(loc)
        out["Points"] = {"Point": points}
        for p in points:
            _push(p["Latitude"], p["Longitude"])

    elif loc_type == "Arrow":
        head_el = loc.find(k("Arrowhead"))
        if head_el is not None:
            head = point_dict(head_el)
            out["Arrowhead"] = head
            _push(head["Latitude"], head["Longitude"])
        points = collect_points(loc)
        out["Points"] = {"Point": points}
        for p in points:
            _push(p["Latitude"], p["Longitude"])

    elif loc_type == "TwoPointLine":
        start_el = loc.find(k("StartPoint"))
        end_el = loc.find(k("Endpoint"))
        if start_el is not None:
            out["StartPoint"] = point_dict(start_el)
            _push(out["StartPoint"]["Latitude"], out["StartPoint"]["Longitude"])
        if end_el is not None:
            out["Endpoint"] = point_dict(end_el)
            _push(out["Endpoint"]["Latitude"], out["Endpoint"]["Longitude"])

    elif loc_type == "Circle":
        center_el = loc.find(k("Center")) or loc
        lat = find_text(center_el, "Latitude")
        lon = find_text(center_el, "Longitude")
        radius = find_text(loc, "Radius")
        if lat:
            out["Latitude"] = lat
        if lon:
            out["Longitude"] = lon
        if radius:
            out["Radius"] = radius
        _push(lat, lon)

    elif loc_type == "Point":
        lat = find_text(loc, "Latitude")
        lon = find_text(loc, "Longitude")
        out["Latitude"] = lat
        out["Longitude"] = lon
        alt_el = loc.find(k("Altitude"))
        if alt_el is not None:
            out["Altitude"] = {
                "Type": find_text(alt_el, "Type"),
                "Value": find_text(alt_el, "Value"),
            }
        _push(lat, lon)

    else:
        # Unknown geometry: capture children verbatim as best-effort.
        for child in loc:
            tag = child.tag.split("}")[-1]
            if child.text and child.text.strip():
                out[tag] = child.text.strip()
        lat = find_text(loc, "Latitude")
        lon = find_text(loc, "Longitude")
        if lat and lon:
            _push(lat, lon)

    return out, pts


def centroid(points: list[tuple[float, float]]) -> tuple[float | None, float | None]:
    if not points:
        return None, None
    n = len(points)
    return (
        sum(p[0] for p in points) / n,
        sum(p[1] for p in points) / n,
    )


def custom_attr(sym: ET.Element, key: str) -> str:
    for entry in sym.findall(f"k:CustomAttributes/k:CustomAttributeEntry", NS):
        if find_text(entry, "Key") == key:
            return find_text(entry, "Value")
    return ""


def id_from_sym(sym: ET.Element) -> str:
    """Prefer a CPCE/TSkey GUID; fall back to FirstLong/SecondLong."""
    tskey = custom_attr(sym, "TSkey")
    if tskey:
        return tskey
    id_el = sym.find(k("Id"))
    if id_el is not None:
        first = find_text(id_el, "FirstLong")
        second = find_text(id_el, "SecondLong")
        if first or second:
            return f"{first}:{second}"
    return ""


def symbol_to_record(sym: ET.Element) -> dict[str, Any] | None:
    sym_type = (sym.get(XSI_TYPE) or "").split(":")[-1]

    loc_el = sym.find(k("Location"))
    if loc_el is None:
        return None
    location, pts = location_to_dict(loc_el)
    lat, lon = centroid(pts)

    reported = find_text(sym, "Report", "Reported") or find_text(sym, "Timestamp")
    symbol_code = find_text(sym, "SymbolCode", "SymbolCodeString")

    record: dict[str, Any] = {
        "Id": id_from_sym(sym),
        "SymbolType": sym_type,
        "Name": find_text(sym, "Name"),
        "AbbreviatedName": find_text(sym, "AbbreviatedName"),
        "LeftOrganisation": find_text(sym, "LeftOrganisation"),
        "RightOrganisation": find_text(sym, "RightOrganisation"),
        "Occupant": find_text(sym, "Occupant"),
        "SymbolCode": symbol_code,
        "OperationalStatus": find_text(sym, "OperationalStatus"),
        "GeopoliticalAffiliation": find_text(sym, "GeopoliticalAffiliation"),
        "Priority": find_text(sym, "Priority"),
        "LocationType": location.get("@xsi:type", ""),
        "Latitude": lat,
        "Longitude": lon,
        "StaffComments": find_text(sym, "StaffComments"),
        "Comment": find_text(sym, "Report", "Comment"),
        "MessageDTG": reported,
        "SigAct": "",
        "UnitPosture": "",
        "UnitStrength": "",
        "Location": location,
    }
    return record


def convert(slf_path: str) -> tuple[str, list[dict[str, Any]]]:
    tree = ET.parse(slf_path)
    root = tree.getroot()

    layer_name = ""
    layer_el = root.find(".//k:Layer", NS)
    if layer_el is not None:
        layer_name = find_text(layer_el, "Name")

    records: list[dict[str, Any]] = []
    for sym in root.findall(".//k:Symbol", NS):
        rec = symbol_to_record(sym)
        if rec is not None:
            records.append(rec)
    return layer_name, records


def write_json(records: list[dict[str, Any]], path: str) -> None:
    with open(path, "w", encoding="utf-8") as f:
        json.dump(records, f, ensure_ascii=False, indent=2)


def write_ndjson(records: list[dict[str, Any]], path: str, index: str) -> None:
    with open(path, "w", encoding="utf-8") as f:
        for i, rec in enumerate(records, start=1):
            meta = {"index": {"_index": index, "_id": rec.get("Id") or str(i)}}
            f.write(json.dumps(meta, ensure_ascii=False) + "\n")
            f.write(json.dumps(rec, ensure_ascii=False) + "\n")


def slugify(name: str) -> str:
    return "".join(c if c.isalnum() else "_" for c in name).strip("_").lower() or "layer"


def main() -> None:
    parser = argparse.ArgumentParser(description="Convert a CPCE .slf file to JSON.")
    parser.add_argument("input", help="Path to the input .slf file")
    parser.add_argument("output", nargs="?", help="Output JSON path")
    parser.add_argument(
        "--ndjson",
        action="store_true",
        help="Emit Elasticsearch bulk NDJSON instead of a JSON array",
    )
    parser.add_argument(
        "--index",
        default=None,
        help="Elasticsearch index name for --ndjson (defaults to slugified layer name)",
    )
    args = parser.parse_args()

    layer_name, records = convert(args.input)

    base, _ = os.path.splitext(args.input)
    if args.output:
        out_path = args.output
    else:
        out_path = f"{base}.ndjson" if args.ndjson else f"{base}.json"

    if args.ndjson:
        index = args.index or slugify(layer_name) or slugify(os.path.basename(base))
        write_ndjson(records, out_path, index)
    else:
        write_json(records, out_path)

    print(
        f"Layer: {layer_name!r}  symbols: {len(records)}  -> {out_path}",
        file=sys.stderr,
    )


if __name__ == "__main__":
    main()
