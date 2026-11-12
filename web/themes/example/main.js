import './public.css';
import {mountPublicPage} from '../../src/public/page.js';
// Shared cards honor optional public fields, including plan details.
import {createCard, skeletonCards} from '../default/cards.js';
import {createConstellation} from '../default/constellation.js';

mountPublicPage({createCard, skeletonCards, createConstellation});
