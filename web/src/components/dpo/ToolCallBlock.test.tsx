import { cleanup, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';

import { ToolCallBlock } from './ToolCallBlock';

afterEach(cleanup);

// The argv line is the field a reviewer reads to see exactly what the
// platform executed, and the field the crystalliser promotes into a
// declaration. A ToolCallBlock that rendered only the argument bag would
// hide the one piece of evidence the runbook is built from.
describe('ToolCallBlock', () => {
  it('renders the executed argv verbatim', () => {
    render(
      <ToolCallBlock
        name="host.restart_service"
        args='{"unit":"nginx.service"}'
        argv={['systemctl', 'restart', 'nginx.service']}
        status="success"
      />,
    );
    expect(screen.getByText('systemctl restart nginx.service')).toBeTruthy();
  });

  it('omits the argv line when no vector ran', () => {
    render(<ToolCallBlock name="pg.vacuum_analyze" status="success" />);
    expect(screen.queryByText(/^\$/)).toBeNull();
  });
});
