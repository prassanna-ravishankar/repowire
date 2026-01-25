'use client';

import { useState } from 'react';
import { Check, Copy } from 'lucide-react';

export default function Installation() {
  const [copied, setCopied] = useState(false);

  const copyToClipboard = () => {
    navigator.clipboard.writeText('uv tool install repowire');
    setCopied(true);
    setTimeout(() => setCopied(false), 2000);
  };

  return (
    <div id="installation" className="bg-gray-50 dark:bg-gray-900 py-16 sm:py-24">
      <div className="max-w-7xl mx-auto px-4 sm:px-6 lg:px-8 text-center">
        <h2 className="text-3xl font-extrabold text-gray-900 dark:text-white sm:text-4xl mb-4">
          Get Started in Seconds
        </h2>
        <p className="text-xl text-gray-500 dark:text-gray-400 mb-8">
          Install with pip or uv. Requires macOS/Linux and Python 3.10+.
        </p>
        
        <div className="max-w-2xl mx-auto relative rounded-lg bg-gray-800 p-4 shadow-2xl">
          <div className="flex items-center justify-between font-mono text-sm sm:text-base text-gray-300">
            <div className="flex items-center">
              <span className="text-green-400 mr-2">$</span>
              <span>uv tool install repowire</span>
            </div>
            <button
              onClick={copyToClipboard}
              className="ml-4 p-2 rounded-md hover:bg-gray-700 transition-colors focus:outline-none focus:ring-2 focus:ring-blue-500"
              aria-label="Copy to clipboard"
            >
              {copied ? (
                <Check className="w-5 h-5 text-green-400" />
              ) : (
                <Copy className="w-5 h-5 text-gray-400" />
              )}
            </button>
          </div>
        </div>

        <div className="mt-8 text-sm text-gray-500">
          <p>
            Alternatively: <code>pip install repowire</code>
          </p>
          <p className="mt-2">
            See the <a href="https://github.com/prassanna-ravishankar/repowire" className="text-blue-500 hover:underline">documentation</a> for full setup instructions.
          </p>
        </div>
      </div>
    </div>
  );
}
