export const DEFAULT_PRICING_UPDATE_URL = 'https://raw.githubusercontent.com/MartialBE/one-api/prices/prices.json';

export const createPricingFetchController = () => {
  let generation = 0;

  return {
    begin() {
      generation += 1;
      return generation;
    },
    invalidate() {
      generation += 1;
    },
    isCurrent(requestGeneration) {
      return requestGeneration === generation;
    },
    accept(requestGeneration, pricing) {
      if (requestGeneration !== generation || !Array.isArray(pricing)) {
        return null;
      }
      return pricing;
    }
  };
};

export const resolveDefaultPricingUrl = (serviceUrl) => {
  if (typeof serviceUrl === 'string' && serviceUrl.trim() !== '') {
    return serviceUrl;
  }
  return DEFAULT_PRICING_UPDATE_URL;
};

export const canAcceptDefaultPricingUrl = ({ controller, generation, currentUrl, cachedUrl, userEdited }) =>
  controller?.isCurrent(generation) && !userEdited && !currentUrl && !cachedUrl;
