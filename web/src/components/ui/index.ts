// Barrel for the visual-uniformity primitives. Pages import from here so
// they don't drift on individual className strings; see web/src/components
// /ui/* for the implementations.
export { Card } from './Card';
export { Chip, type ChipTone } from './Chip';
export { DependencyNotice, classifyDependencyError } from './DependencyNotice';
export { EmptyState } from './EmptyState';
export { IncidentStatusBadge, SeverityBadge } from './IncidentBadges';
export { LoadingState } from './LoadingState';
export { PageHeader } from './PageHeader';
export { Button } from './Button';
export { RoleSelect, type RoleFilterValue } from './RoleSelect';
export { ConfirmProvider, useConfirm, type ConfirmOptions } from './confirm';
