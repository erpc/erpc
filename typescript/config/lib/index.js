"use strict";
var __defProp = Object.defineProperty;
var __getOwnPropDesc = Object.getOwnPropertyDescriptor;
var __getOwnPropNames = Object.getOwnPropertyNames;
var __hasOwnProp = Object.prototype.hasOwnProperty;
var __export = (target, all) => {
  for (var name in all)
    __defProp(target, name, { get: all[name], enumerable: true });
};
var __copyProps = (to, from, except, desc) => {
  if (from && typeof from === "object" || typeof from === "function") {
    for (let key of __getOwnPropNames(from))
      if (!__hasOwnProp.call(to, key) && key !== except)
        __defProp(to, key, { get: () => from[key], enumerable: !(desc = __getOwnPropDesc(from, key)) || desc.enumerable });
  }
  return to;
};
var __toCommonJS = (mod) => __copyProps(__defProp({}, "__esModule", { value: true }), mod);

// src/index.ts
var src_exports = {};
__export(src_exports, {
  DataFinalityStateFinalized: () => DataFinalityStateFinalized,
  DataFinalityStateUnfinalized: () => DataFinalityStateUnfinalized,
  DataFinalityStateUnknown: () => DataFinalityStateUnknown,
  createConfig: () => createConfig,
  initErpcConfig: () => initErpcConfig
});
module.exports = __toCommonJS(src_exports);

// src/generated.ts
var DataFinalityStateFinalized = 0;
var DataFinalityStateUnfinalized = 1;
var DataFinalityStateUnknown = 2;

// src/builder/index.ts
var ConfigBuilder = class {
  constructor(config) {
    this.config = config;
  }
  /**
   * Adds rate limiters to the configuration.
   * @param budgets A record where keys are budget identifiers and values are arrays of RateLimitRuleConfig.
   */
  addRateLimiters(budgets) {
    const mappedRateLimiters = Object.entries(
      budgets
    ).map(([id, rules]) => ({
      id,
      rules
    }));
    const rateLimiters = this.config.rateLimiters ?? { budgets: [] };
    rateLimiters.budgets.push(...mappedRateLimiters);
    this.config = {
      ...this.config,
      rateLimiters
    };
    return this;
  }
  /**
   * Adds a project to the configuration.
   * @param project The project configuration.
   */
  addProject(project) {
    const newProjects = [
      ...this.config.projects || [],
      project
    ];
    this.config = {
      ...this.config,
      projects: newProjects
    };
    return this;
  }
  /**
   * Builds the final configuration.
   */
  build() {
    return this.config;
  }
};
function initErpcConfig(baseConfig) {
  return new ConfigBuilder(baseConfig);
}

// src/index.ts
var createConfig = (cfg) => {
  return cfg;
};
// Annotate the CommonJS export names for ESM import in node:
0 && (module.exports = {
  DataFinalityStateFinalized,
  DataFinalityStateUnfinalized,
  DataFinalityStateUnknown,
  createConfig,
  initErpcConfig
});
//# sourceMappingURL=index.js.map
