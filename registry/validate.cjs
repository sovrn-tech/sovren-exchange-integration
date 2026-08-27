#!/usr/bin/env node
// Validate the registry metadata against the pinned cosmos/chain-registry
// draft-07 schemas in ./schemas. Requires ajv@8 + ajv-formats@2 to be
// resolvable (local node_modules or NODE_PATH), e.g.:
//   npm install --no-save --prefix /tmp/ajv ajv@8 ajv-formats@2
//   NODE_PATH=/tmp/ajv/node_modules node registry/validate.cjs
// Exit: 0 all valid, 1 validation failure, 2 missing deps/files.

const fs = require("fs");
const path = require("path");

let Ajv, addFormats;
try {
  Ajv = require("ajv");
  addFormats = require("ajv-formats");
} catch (e) {
  console.error("validate.cjs: ajv/ajv-formats not resolvable:", e.message);
  process.exit(2);
}

const reg = path.dirname(__filename);
const pairs = [
  ["chain.schema.json", ["chain.json", "testnets/sovrtestnet/chain.json"]],
  ["assetlist.schema.json", ["assetlist.json", "testnets/sovrtestnet/assetlist.json"]],
  ["versions.schema.json", ["versions.json"]],
];

let failed = 0;
for (const [schemaFile, dataFiles] of pairs) {
  const ajv = new Ajv({ strict: false, allErrors: true });
  addFormats(ajv);
  // The pinned schemas declare the https draft-07 meta-schema URI; ajv
  // registers only the http+fragment form by default.
  ajv.addMetaSchema(
    require("ajv/dist/refs/json-schema-draft-07.json"),
    "https://json-schema.org/draft-07/schema",
  );
  const schemaPath = path.join(reg, "schemas", schemaFile);
  if (!fs.existsSync(schemaPath)) {
    console.error(`validate.cjs: missing pinned schema ${schemaPath}`);
    process.exit(2);
  }
  const validate = ajv.compile(JSON.parse(fs.readFileSync(schemaPath, "utf8")));
  for (const df of dataFiles) {
    const dataPath = path.join(reg, df);
    if (!fs.existsSync(dataPath)) {
      console.error(`FAIL ${df}: file missing`);
      failed++;
      continue;
    }
    const data = JSON.parse(fs.readFileSync(dataPath, "utf8"));
    if (validate(data)) {
      console.log(`OK   ${df}`);
    } else {
      failed++;
      console.error(`FAIL ${df}`);
      console.error(JSON.stringify(validate.errors, null, 1));
    }
  }
}
process.exit(failed ? 1 : 0);
